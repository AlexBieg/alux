package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// The production redirect URL, so the tests exercise the same value that is registered with
// real servers.
var testRedirect = redirectURL

// Stands in for opening a browser and the user approving. Follows the authorize URL by hand and
// reads the code off the redirect, so PKCE and state are exercised for real.
func fetchCode(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequestWithContext(ctx, "GET", args.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return nil, err
	}
	q := loc.Query()
	if e := q.Get("error"); e != "" {
		return nil, fmt.Errorf("authorization refused: %s: %s", e, q.Get("error_description"))
	}
	return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
}

type rig struct {
	as  *fakeAS
	res *fakeResource
	dir string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	as := newFakeAS()
	as.AccessTokenTTL = 3600
	t.Cleanup(as.Close)
	res := newFakeResource(as)
	t.Cleanup(res.Close)
	return &rig{as: as, res: res, dir: t.TempDir()}
}

// connect builds a handler the way an alux launch would and connects to the protected server.
func (r *rig) connect(t *testing.T, guard bool) (*mcp.ClientSession, *fileTokenSource, int) {
	session, ts, browsed, _ := r.connectReporting(t, guard)
	return session, ts, browsed
}

// connectReporting is connect, also returning the scopes a stale registration was found to be
// missing. Separate so the existing callers keep their three-value shape.
func (r *rig) connectReporting(t *testing.T, guard bool) (*mcp.ClientSession, *fileTokenSource, int, []string) {
	t.Helper()
	browsed := 0
	var stale []string
	h, ts, err := newHandler(handlerOpts{
		Dir:       r.dir,
		Server:    "fake",
		Resource:  r.res.URL + "/mcp",
		Issuer:    r.as.URL,
		Redirect:  testRedirect,
		Guard:     guard,
		HTTP:      http.DefaultClient,
		OnBrowser: func() { browsed++ },
		OnStale:   func(missing []string) { stale = append(stale, missing...) },
		Fetch:     fetchCode,
	})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "alux", Version: "0.1.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:     r.res.URL + "/mcp",
		OAuthHandler: h,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	return session, ts, browsed, stale
}

func (r *rig) callEcho(t *testing.T, session *mcp.ClientSession) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "hello"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out.IsError {
		t.Fatalf("CallTool returned an error result: %+v", out.Content)
	}
	var got echoOutput
	raw, _ := json.Marshal(out.StructuredContent)
	_ = json.Unmarshal(raw, &got)
	return got.Text
}

// 1. Cold start: no saved state, so the client registers dynamically, signs in, and connects.
func TestColdStartRegistersAndSignsIn(t *testing.T) {
	r := newRig(t)
	session, _, browsed := r.connect(t, true)

	if got := r.callEcho(t, session); got != "hello" {
		t.Fatalf("echo = %q, want %q", got, "hello")
	}
	if n := r.as.Registers.Load(); n != 1 {
		t.Errorf("dynamic registrations = %d, want 1", n)
	}
	if browsed != 1 {
		t.Errorf("browser sign-ins = %d, want 1", browsed)
	}
	if n := r.res.Unauthorized.Load(); n < 1 {
		t.Errorf("401s served = %d, want at least 1 to trigger the flow", n)
	}

	saved := newStore(r.dir, "fake").load()
	if saved.ClientID == "" {
		t.Error("client registration was not persisted, so every launch would re-register")
	}
	if saved.Token == nil || saved.Token.RefreshToken == "" {
		t.Error("tokens were not persisted, so every launch would sign in again")
	}
	if saved.TokenURL == "" {
		t.Error("token endpoint was not persisted; a warm start could not refresh")
	}
}

// 2. Warm start: a second alux launch reuses the file. This is the claim that matters for the
// port — no re-registration, no browser.
func TestWarmStartReusesRegistrationAndTokens(t *testing.T) {
	r := newRig(t)
	s1, _, _ := r.connect(t, true)
	r.callEcho(t, s1)
	s1.Close()

	registersAfterCold := r.as.Registers.Load()

	s2, _, browsed := r.connect(t, true)
	if got := r.callEcho(t, s2); got != "hello" {
		t.Fatalf("echo after warm start = %q", got)
	}
	if browsed != 0 {
		t.Errorf("browser sign-ins on warm start = %d, want 0", browsed)
	}
	if n := r.as.Registers.Load(); n != registersAfterCold {
		t.Errorf("registrations grew from %d to %d on warm start", registersAfterCold, n)
	}
}

// 2b. The case where the saved registration actually earns its keep. A warm start with a dead
// refresh token has to sign in again — and must do it as the SAME client, not mint a new
// client_id on every session expiry. The happy warm start above cannot catch this, because it
// never reaches the registration step at all.
func TestReauthorizationAfterRevocationReusesRegistration(t *testing.T) {
	r := newRig(t)
	s1, _, _ := r.connect(t, true)
	r.callEcho(t, s1)
	s1.Close()

	registersAfterCold := r.as.Registers.Load()
	expireSavedToken(t, r.dir)
	r.as.RevokeRefreshTokens()

	s2, _, browsed := r.connect(t, true)
	if got := r.callEcho(t, s2); got != "hello" {
		t.Fatalf("echo after re-authorization = %q", got)
	}
	if browsed != 1 {
		t.Errorf("browser sign-ins after revocation = %d, want 1", browsed)
	}
	if n := r.as.Registers.Load(); n != registersAfterCold {
		t.Errorf("re-registered on revocation: registrations went %d -> %d; the saved client_id was not reused",
			registersAfterCold, n)
	}
}

// The recovery in 2b is conditional on something easy to break by accident: the SDK only skips the
// Authorization header and lets the 401 drive re-authorization when the token source's error
// unwraps to an *oauth2.RetrieveError with code invalid_grant. Wrapping with %v instead of %w in
// fileTokenSource.Token would swallow that and turn a recoverable expiry into a hard failure.
func TestRefreshFailureStaysUnwrappable(t *testing.T) {
	r := newRig(t)
	s, _, _ := r.connect(t, true)
	r.callEcho(t, s)
	s.Close()
	expireSavedToken(t, r.dir)
	r.as.RevokeRefreshTokens()

	a, _ := twoSources(t, r, true)
	_, err := a.Token()
	if err == nil {
		t.Fatal("expected the refresh to fail against revoked tokens")
	}
	var retrieveErr *oauth2.RetrieveError
	if !errors.As(err, &retrieveErr) {
		t.Fatalf("error does not unwrap to *oauth2.RetrieveError, so the SDK cannot re-authorize: %v", err)
	}
	if retrieveErr.ErrorCode != "invalid_grant" {
		t.Errorf("ErrorCode = %q, want invalid_grant", retrieveErr.ErrorCode)
	}
}

// expire rewrites the saved token so the next use must refresh, which is what two alux instances waking
// up against a stale access token look like.
func expireSavedToken(t *testing.T, dir string) {
	t.Helper()
	st := newStore(dir, "fake")
	v := st.load()
	v.Token.Expiry = time.Now().Add(-time.Hour)
	if err := st.save(v); err != nil {
		t.Fatal(err)
	}
}

// twoSources builds the token sources two concurrently-starting alux instances would hold: same file,
// same expired access token, same refresh token.
func twoSources(t *testing.T, r *rig, guard bool) (*fileTokenSource, *fileTokenSource) {
	t.Helper()
	var out []*fileTokenSource
	for range 2 {
		_, ts, err := newHandler(handlerOpts{
			Dir: r.dir, Server: "fake", Issuer: r.as.URL, Redirect: testRedirect,
			Guard: guard, HTTP: http.DefaultClient, Fetch: fetchCode,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ts == nil {
			t.Fatal("warm start produced no token source")
		}
		out = append(out, ts)
	}
	return out[0], out[1]
}

// raceRefresh drives both sources into Token() at once. The pause inside the check-then-refresh
// window is what makes the outcome depend on the lock: without one, both sources sit in that
// window together and both go on to spend the same refresh token.
func raceRefresh(a, b *fileTokenSource) []error {
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, ts := range []*fileTokenSource{a, b} {
		ts.preRefresh = func() { time.Sleep(50 * time.Millisecond) }
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = ts.Token()
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// 3. The bug, reproduced. Two alux instances refresh the same rotated token; the loser is told its
// refresh token is invalid. This is what took down every running alux on 2026-09-28.
func TestConcurrentRefreshWithoutGuardLosesOneClient(t *testing.T) {
	r := newRig(t)
	s, _, _ := r.connect(t, true)
	r.callEcho(t, s)
	s.Close()
	expireSavedToken(t, r.dir)

	a, b := twoSources(t, r, false)
	errs := raceRefresh(a, b)

	failed := 0
	for _, err := range errs {
		if err != nil {
			failed++
			if !strings.Contains(err.Error(), "invalid_grant") {
				t.Errorf("unexpected failure: %v", err)
			}
		}
	}
	if failed != 1 {
		t.Fatalf("expected exactly one client to lose the race, got %d failures (%v)", failed, errs)
	}
	if n := r.as.Rejected.Load(); n != 1 {
		t.Errorf("server rejected %d reused refresh tokens, want 1", n)
	}
	t.Logf("unguarded: %d/2 clients lost their session and would fall back to a browser sign-in", failed)
}

// 4. The fix. Same race, with the lock-and-adopt guard: both clients end up with a working token
// and only one refresh is spent.
func TestConcurrentRefreshWithGuardKeepsBothClients(t *testing.T) {
	r := newRig(t)
	s, _, _ := r.connect(t, true)
	r.callEcho(t, s)
	s.Close()
	expireSavedToken(t, r.dir)

	before := r.as.Refreshes.Load()
	a, b := twoSources(t, r, true)
	errs := raceRefresh(a, b)

	for i, err := range errs {
		if err != nil {
			t.Errorf("client %d failed under the guard: %v", i, err)
		}
	}
	if n := r.as.Rejected.Load(); n != 0 {
		t.Errorf("server rejected %d reused refresh tokens, want 0", n)
	}
	if spent := r.as.Refreshes.Load() - before; spent != 1 {
		t.Errorf("refresh grants spent = %d, want 1", spent)
	}
	if adopted := a.Adopted.Load() + b.Adopted.Load(); adopted != 1 {
		t.Errorf("sibling-token adoptions = %d, want 1", adopted)
	}

	// Both must agree on the token that is now on disk, or the next request 401s.
	ta, _ := a.Token()
	tb, _ := b.Token()
	if ta.AccessToken != tb.AccessToken {
		t.Errorf("clients diverged: %q vs %q", ta.AccessToken, tb.AccessToken)
	}
	if disk := newStore(r.dir, "fake").load().Token; disk == nil || disk.AccessToken != ta.AccessToken {
		t.Errorf("disk holds a different token than the clients")
	}
}

// A server that adds a scope after a client registered leaves that client registered for the old
// set, while the SDK goes on requesting every advertised scope. The authorization server refuses
// the difference — invalid_scope, "Unsupported or unregistered scope(s)" — and alux has to
// notice the registration is stale and make a new one.
//
// Observed against a real MCP server on 2026-10-01: a client registered for four scopes, the server
// added conductor:write, and every sign-in failed with exactly that error until the saved
// registration was deleted by hand.
func TestAnAddedServerScopeForcesReregistration(t *testing.T) {
	r := newRig(t)

	// A normal cold start, registering for what the server advertises today.
	session, _, browsed := r.connect(t, true)
	if got := r.callEcho(t, session); got != "hello" {
		t.Fatalf("echo = %q, want %q", got, "hello")
	}
	if browsed != 1 || r.as.Registers.Load() != 1 {
		t.Fatalf("cold start: %d sign-ins, %d registrations; want 1 and 1", browsed, r.as.Registers.Load())
	}
	session.Close()

	// The server grows a capability, and the saved token goes cold — which is the only state in
	// which the authorization server is consulted again.
	r.res.AddScope("mcp:admin")
	expireSavedToken(t, r.dir)

	session2, _, browsed2, stale := r.connectReporting(t, true)
	if got := r.callEcho(t, session2); got != "hello" {
		t.Errorf("echo after re-registration = %q, want %q", got, "hello")
	}
	if len(stale) != 1 || stale[0] != "mcp:admin" {
		t.Errorf("reported missing scopes = %v, want [mcp:admin]", stale)
	}
	if n := r.as.Registers.Load(); n != 2 {
		t.Errorf("registrations = %d, want 2 — the stale one should have been replaced", n)
	}
	if browsed2 != 1 {
		t.Errorf("sign-ins on the second connect = %d, want 1", browsed2)
	}

	// And the new registration is recorded, so the next start does not do this again.
	saved := newStore(r.dir, "fake").load()
	if !slices.Contains(saved.Scopes, "mcp:admin") {
		t.Errorf("saved scopes = %v, want the new scope among them", saved.Scopes)
	}
}

// The failure as it actually happened, which the test above does not reach: a stale registration
// matters most when the refresh token is dead too, because that is the state that forces a fresh
// authorization — the only request that carries scopes. Stale registration plus dead refresh is
// what produced "Unsupported or unregistered scope(s): conductor:write" at startup, and it is an
// unrecoverable wedge rather than a degraded start: every launch fails the same way until the
// saved registration is deleted by hand.
//
// Without the staleness check this test does not merely assert less — it cannot connect at all.
func TestAStaleRegistrationWithADeadRefreshStillRecovers(t *testing.T) {
	r := newRig(t)
	s1, _, _ := r.connect(t, true)
	r.callEcho(t, s1)
	s1.Close()

	// The three conditions together: the server gained a scope, the access token expired, and the
	// refresh token is gone. Nothing short of re-registering gets out of this.
	r.res.AddScope("mcp:admin")
	expireSavedToken(t, r.dir)
	r.as.RevokeRefreshTokens()

	s2, _, browsed, stale := r.connectReporting(t, true)
	if got := r.callEcho(t, s2); got != "hello" {
		t.Fatalf("echo after recovery = %q, want %q", got, "hello")
	}
	if len(stale) != 1 || stale[0] != "mcp:admin" {
		t.Errorf("reported missing scopes = %v, want [mcp:admin]", stale)
	}
	if browsed != 1 {
		t.Errorf("sign-ins = %d, want 1", browsed)
	}
	if n := r.as.Registers.Load(); n != 2 {
		t.Errorf("registrations = %d, want 2", n)
	}
}

// The check costs a network call, so it must not happen while the saved token is still good. A
// live token is proof the registration behind it works, and a sign-in nobody asked for is a worse
// outcome than a scope gained late.
func TestALiveTokenIsNotDisturbedByAnAddedScope(t *testing.T) {
	r := newRig(t)
	r.as.AccessTokenTTL = 3600

	session, _, _ := r.connect(t, true)
	r.callEcho(t, session)
	session.Close()

	r.res.AddScope("mcp:admin")

	_, _, browsed, stale := r.connectReporting(t, true)
	if len(stale) != 0 {
		t.Errorf("a valid token was re-registered over: missing = %v", stale)
	}
	if browsed != 0 {
		t.Errorf("sign-ins = %d, want 0 — the saved token was still good", browsed)
	}
	if n := r.as.Registers.Load(); n != 1 {
		t.Errorf("registrations = %d, want 1", n)
	}
}

// Discovery failing must not cost a sign-in. An unreachable metadata endpoint yields no advertised
// scopes, and nothing missing, so the saved registration stands.
func TestUnreachableMetadataDoesNotDiscardTheRegistration(t *testing.T) {
	if missing := missingScopes([]string{"mcp"}, nil); len(missing) != 0 {
		t.Errorf("missing = %v, want none when discovery returns nothing", missing)
	}
	if missing := missingScopes(nil, nil); len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
}

// offline_access is added by the SDK rather than requested from a registration, and no server
// gates it, so finding it advertised must never invalidate a registration.
func TestOfflineAccessIsNotTreatedAsAMissingScope(t *testing.T) {
	if missing := missingScopes([]string{"mcp"}, []string{"mcp", "offline_access"}); len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
}

func TestStorePermissions(t *testing.T) {
	dir := t.TempDir()
	st := newStore(dir, "fake")
	if err := st.save(stored{ClientID: "x"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
}
