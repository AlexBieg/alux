package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// One 0600 JSON file per server under a shared auth dir, holding the dynamic client registration
// and the current tokens, so a second alux starting later reuses both instead of
// re-registering and re-prompting.
//
// The Go SDK has no credential-store interface — persistence has to be built out of two hooks on
// AuthorizationCodeHandlerConfig (InitialTokenSource, NewTokenSource) plus PreregisteredClient.

// Persisted per server. Endpoint is saved alongside the client because a warm start never runs
// discovery: the handler hands back the saved token source and is never asked to Authorize, so
// nothing would otherwise tell us the token URL to refresh against.
type stored struct {
	ClientID     string        `json:"client_id,omitempty"`
	ClientSecret string        `json:"client_secret,omitempty"`
	Issuer       string        `json:"issuer,omitempty"`
	AuthURL      string        `json:"auth_url,omitempty"`
	TokenURL     string        `json:"token_url,omitempty"`
	Scopes       []string      `json:"scopes,omitempty"`
	Token        *oauth2.Token `json:"token,omitempty"`
}

type store struct {
	path string
}

func newStore(dir, server string) *store {
	return &store{path: filepath.Join(dir, server+".json")}
}

func (s *store) load() stored {
	var out stored
	data, err := os.ReadFile(s.path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

func (s *store) save(v stored) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// Held across the read-refresh-write sequence so two processes cannot both spend the same refresh
// token. A separate file because the token file is rewritten wholesale.
func (s *store) lock() (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// fileTokenSource owns the current token rather than delegating to a long-lived
// cfg.TokenSource(ctx, tok). That is forced, not stylistic: oauth2's refresher captures the token
// it was constructed with and offers no way to replace it, so a source that delegated could never
// adopt a token a sibling process had just written. Owning it means building a one-shot refresher
// per refresh.
type fileTokenSource struct {
	store *store
	cfg   *oauth2.Config
	ctx   context.Context

	// Guard switches on the cross-process protection. Off, this is the naive implementation that
	// every "just persist the tokens" port would arrive at first.
	Guard bool

	// preRefresh widens the check-then-refresh window so a test can force two processes to
	// interleave inside it. Against a local server that window is microseconds wide, so without
	// this a missing lock looks indistinguishable from a working one.
	preRefresh func()

	mu  sync.Mutex
	cur *oauth2.Token

	Refreshed atomic.Int32 // refreshes this source actually performed
	Adopted   atomic.Int32 // times it took a sibling's token instead of refreshing
}

func (s *fileTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cur.Valid() {
		return s.cur, nil
	}

	if s.Guard {
		release, err := s.store.lock()
		if err != nil {
			return nil, err
		}
		defer release()
		// Whoever held the lock may have refreshed while we waited, in which case our refresh token
		// is already spent and the good one is on disk.
		if disk := s.store.load().Token; disk != nil && disk.Valid() && disk.AccessToken != s.cur.AccessToken {
			s.cur = disk
			s.Adopted.Add(1)
			return disk, nil
		}
	}

	if s.preRefresh != nil {
		s.preRefresh()
	}

	s.Refreshed.Add(1)
	tok, err := s.cfg.TokenSource(s.ctx, s.cur).Token()
	if err != nil {
		return nil, fmt.Errorf("refresh failed: %w", err)
	}
	s.cur = tok
	s.persist(tok)
	return tok, nil
}

func (s *fileTokenSource) persist(tok *oauth2.Token) {
	v := s.store.load()
	v.ClientID = s.cfg.ClientID
	v.ClientSecret = s.cfg.ClientSecret
	v.AuthURL = s.cfg.Endpoint.AuthURL
	v.TokenURL = s.cfg.Endpoint.TokenURL
	v.Scopes = s.cfg.Scopes
	v.Token = tok
	_ = s.store.save(v)
}

// handlerOpts is everything the caller varies between scenarios.
type handlerOpts struct {
	Dir       string
	Server    string
	Resource  string // the MCP server's URL
	Issuer    string
	Redirect  string
	Guard     bool
	HTTP      *http.Client
	OnBrowser func() // called when an interactive sign-in would be needed
	// OnStale is called with the scopes a saved registration lacks, just before it is discarded.
	// Worth announcing because the consequence is a browser sign-in the user did not ask for.
	OnStale func(missing []string)
	Fetch   auth.AuthorizationCodeFetcher
}

// A saved registration is good forever, but the scopes it covers are only the ones the server
// advertised on the day it was made. When a server adds one, the SDK starts requesting it — it
// asks for every advertised scope at sign-in — while the client is still registered for the old
// set, and the authorization server refuses the difference:
//
//	invalid_scope: Unsupported or unregistered scope(s): conductor:write
//
// Which reads like a server fault and is a stale client. The registration has to be thrown away
// and remade; there is no way to widen one in place without a registration access token, which
// DCR gives us and we do not keep.
//
// missingScopes is what the saved registration does not cover. Empty advertised means discovery
// failed, and that yields nothing missing: a network blip must not cost the user a sign-in.
func missingScopes(saved []string, advertised []string) []string {
	var missing []string
	for _, scope := range advertised {
		// The SDK adds offline_access by itself whenever a server advertises it, and no server
		// gates it on registration, so it never belongs in this comparison.
		if scope == "offline_access" || slices.Contains(saved, scope) {
			continue
		}
		missing = append(missing, scope)
	}
	return missing
}

// usable reports whether a saved token can still be presented as-is. Only the expiry is in
// question: a token that is merely expired may still refresh, but one this side of its expiry
// needs nothing from the authorization server at all.
func usable(tok *oauth2.Token) bool { return tok != nil && tok.Valid() }

// newHandler builds an auth.OAuthHandler that persists everything it learns. The warm-start path is
// the whole point: with a saved client and token it neither registers nor signs in.
func newHandler(o handlerOpts) (*auth.AuthorizationCodeHandler, *fileTokenSource, error) {
	st := newStore(o.Dir, o.Server)
	saved := st.load()

	// Checked only when the saved token cannot be presented as-is, which keeps a warm start at zero
	// network calls — the property the whole file is arranged around. A live token is proof enough
	// that the registration behind it works, and widening the scope set would cost a sign-in that
	// nothing has asked for. An expired one is about to need the authorization server anyway, and
	// that is the path where a stale registration turns into a refusal.
	//
	// Discarded in memory rather than deleted from disk: if registering again fails, the old file is
	// no worse than the one that was already failing, and a successful registration overwrites it.
	if saved.ClientID != "" && !usable(saved.Token) {
		if missing := missingScopes(saved.Scopes, advertisedScopes(o.Resource, o.HTTP)); len(missing) > 0 {
			if o.OnStale != nil {
				o.OnStale(missing)
			}
			saved = stored{}
		}
	}

	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:         o.Redirect,
		Client:              o.HTTP,
		RequestRefreshToken: true,
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			if o.OnBrowser != nil {
				o.OnBrowser()
			}
			return o.Fetch(ctx, args)
		},
	}

	// Reusing a saved registration is the difference between one client_id forever and a new one on
	// every launch. Priority inside the SDK is CIMD > preregistered > DCR, so supplying this skips DCR.
	if saved.ClientID != "" {
		cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: saved.ClientID, Issuer: saved.Issuer}
	} else {
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:              "alux",
				RedirectURIs:            []string{o.Redirect},
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
				TokenEndpointAuthMethod: "none",
				Scope:                   strings.Join(advertisedScopes(o.Resource, o.HTTP), " "),
			},
		}
	}

	var ts *fileTokenSource

	// Called after a fresh code exchange. It is also the only place the SDK hands back the
	// oauth2.Config it built from the DCR response, which is how the registration gets persisted.
	cfg.NewTokenSource = func(ctx context.Context, c *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		ts = &fileTokenSource{store: st, cfg: c, ctx: ctx, cur: tok, Guard: o.Guard}
		v := st.load()
		v.Issuer = o.Issuer
		_ = st.save(v)
		ts.persist(tok)
		return ts, nil
	}

	// Warm start: a saved token means Authorize is never called, so no discovery, no browser.
	if saved.Token != nil && saved.TokenURL != "" {
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, o.HTTP)
		ts = &fileTokenSource{
			store: st,
			cfg: &oauth2.Config{
				ClientID:     saved.ClientID,
				ClientSecret: saved.ClientSecret,
				Endpoint:     oauth2.Endpoint{AuthURL: saved.AuthURL, TokenURL: saved.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
				RedirectURL:  o.Redirect,
				Scopes:       saved.Scopes,
			},
			ctx:   ctx,
			cur:   saved.Token,
			Guard: o.Guard,
		}
		cfg.InitialTokenSource = ts
	}

	h, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return nil, nil, err
	}
	return h, ts, nil
}

// The SDK requests every advertised scope at sign-in but registers before looking, so register for them up front.
func advertisedScopes(resource string, c *http.Client) []string {
	u, err := url.Parse(resource)
	if err != nil || u.Host == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// RFC 9728's path-inserted location first, then the root.
	base := u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
	for _, metadataURL := range []string{base + strings.TrimSuffix(u.Path, "/"), base} {
		if prm, err := oauthex.GetProtectedResourceMetadata(ctx, metadataURL, resource, c); err == nil {
			return prm.ScopesSupported
		}
	}
	return nil
}
