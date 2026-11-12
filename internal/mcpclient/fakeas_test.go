package mcpclient

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// A fake OAuth 2.1 authorization server that behaves the way the servers alux talks to behave:
// dynamic client registration, PKCE, short-lived access tokens, and — the part that matters —
// refresh tokens that ROTATE and are single-use. Rotation is what makes two clients refreshing at
// once a race rather than a duplicate: the loser's refresh token is already dead when it arrives.
type fakeAS struct {
	*httptest.Server

	AccessTokenTTL int // seconds; 0 means "already expired on arrival"

	mu        sync.Mutex
	clients   map[string]string // registered client_id -> its registered scopes
	codes     map[string]string // auth code -> PKCE challenge
	refresh   map[string]bool   // refresh token -> still valid
	Registers atomic.Int32      // how many times DCR was called
	Refreshes atomic.Int32      // how many refresh grants were attempted
	Rejected  atomic.Int32      // how many refreshes were rejected as already-used
}

func newFakeAS() *fakeAS {
	as := &fakeAS{
		AccessTokenTTL: 1,
		clients:        map[string]string{},
		codes:          map[string]string{},
		refresh:        map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", as.metadata)
	mux.HandleFunc("/register", as.register)
	mux.HandleFunc("/authorize", as.authorize)
	mux.HandleFunc("/token", as.token)
	as.Server = httptest.NewServer(mux)
	return as
}

func (as *fakeAS) metadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                         as.URL,
		"authorization_endpoint":                         as.URL + "/authorize",
		"token_endpoint":                                 as.URL + "/token",
		"registration_endpoint":                          as.URL + "/register",
		"scopes_supported":                               []string{"mcp", "offline_access"},
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// A registration that names no scopes gets a default set, as one real server's does; asking for more at
// sign-in is then refused.
func (as *fakeAS) register(w http.ResponseWriter, r *http.Request) {
	var meta struct {
		Scope string `json:"scope"`
	}
	_ = json.NewDecoder(r.Body).Decode(&meta)
	if meta.Scope == "" {
		meta.Scope = "mcp"
	}
	n := as.Registers.Add(1)
	id := fmt.Sprintf("client-%d", n)
	as.mu.Lock()
	as.clients[id] = meta.Scope
	as.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{
		"client_id":                  id,
		"redirect_uris":              []string{"http://127.0.0.1:0/callback"},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
	})
}

// Stands in for the user approving in a browser: hands back a code on the redirect URI.
func (as *fakeAS) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	if bad := as.unregisteredScopes(q.Get("client_id"), q.Get("scope")); len(bad) > 0 {
		rq := redirect.Query()
		rq.Set("error", "invalid_scope")
		rq.Set("error_description", "Unsupported or unregistered scope(s): "+strings.Join(bad, " "))
		redirect.RawQuery = rq.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
		return
	}
	code := fmt.Sprintf("code-%d", as.Registers.Load())
	as.mu.Lock()
	as.codes[code] = q.Get("code_challenge")
	as.mu.Unlock()

	rq := redirect.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	rq.Set("iss", as.URL)
	redirect.RawQuery = rq.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// offline_access is never refused: the SDK adds it on its own whenever the server advertises it.
func (as *fakeAS) unregisteredScopes(clientID, requested string) []string {
	as.mu.Lock()
	registered := strings.Fields(as.clients[clientID])
	as.mu.Unlock()
	var bad []string
	for _, scope := range strings.Fields(requested) {
		if scope != "offline_access" && !slices.Contains(registered, scope) {
			bad = append(bad, scope)
		}
	}
	return bad
}

func (as *fakeAS) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		as.mu.Lock()
		challenge, ok := as.codes[r.Form.Get("code")]
		delete(as.codes, r.Form.Get("code"))
		as.mu.Unlock()
		if !ok {
			as.oauthError(w, "invalid_grant", "unknown code")
			return
		}
		if pkce(r.Form.Get("code_verifier")) != challenge {
			as.oauthError(w, "invalid_grant", "PKCE verifier does not match challenge")
			return
		}
		as.issue(w, "from-code")

	case "refresh_token":
		n := as.Refreshes.Add(1)
		old := r.Form.Get("refresh_token")
		as.mu.Lock()
		live := as.refresh[old]
		// Rotation: presenting a refresh token consumes it, whether or not this caller wins.
		delete(as.refresh, old)
		as.mu.Unlock()
		if !live {
			as.Rejected.Add(1)
			as.oauthError(w, "invalid_grant", "refresh token already used or revoked")
			return
		}
		as.issue(w, fmt.Sprintf("from-refresh-%d", n))

	default:
		as.oauthError(w, "unsupported_grant_type", "")
	}
}

func (as *fakeAS) issue(w http.ResponseWriter, tag string) {
	access := "access-" + tag
	refresh := "refresh-" + tag
	as.mu.Lock()
	as.refresh[refresh] = true
	as.mu.Unlock()
	writeJSON(w, map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "Bearer",
		"expires_in":    as.AccessTokenTTL,
		"scope":         "mcp offline_access",
	})
}

// RevokeRefreshTokens stands in for a server-side session expiry: the saved refresh token no
// longer works, so the client has to sign in again — the one path where a saved registration
// either gets reused or silently becomes a second client_id.
func (as *fakeAS) RevokeRefreshTokens() {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.refresh = map[string]bool{}
}

// Valid reports whether a bearer token is one this server issued and has not been superseded.
func (as *fakeAS) ValidAccess(token string) bool {
	as.mu.Lock()
	defer as.mu.Unlock()
	// Any access token this server minted is accepted; these tests are about refresh, not introspection.
	return len(token) > len("access-") && token[:len("access-")] == "access-"
}

func (as *fakeAS) oauthError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func pkce(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
