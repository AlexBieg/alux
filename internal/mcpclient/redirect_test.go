package mcpclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestHeaderClientDoesNotSendHeadersToAnotherHost(t *testing.T) {
	var leaked atomic.Value
	leaked.Store("")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"))
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	resp, err := headerClient(map[string]string{"Authorization": "Bearer s3cret"}).Get(origin.URL)
	if err == nil {
		_ = resp.Body.Close()
	}
	if got := leaked.Load().(string); got != "" {
		t.Fatalf("second host received Authorization %q", got)
	}
	if err == nil {
		t.Fatal("cross-host redirect was followed")
	}
}

func TestHeaderClientFollowsASameHostRedirectWithHeaders(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := headerClient(map[string]string{"Authorization": "Bearer s3cret"}).Get(srv.URL + "/old")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "Bearer s3cret" {
		t.Fatalf("redirect target saw Authorization %q", body)
	}
}

func TestOAuthClientDoesNotResendATokenRequestToAnotherHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var reached atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/token", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	cfg := &oauth2.Config{ClientID: "alux", Endpoint: oauth2.Endpoint{TokenURL: origin.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, oauthClient)
	stale := &oauth2.Token{AccessToken: "old", RefreshToken: "refresh-s3cret", Expiry: time.Now().Add(-time.Hour)}
	if _, err := cfg.TokenSource(ctx, stale).Token(); err == nil {
		t.Fatal("refresh succeeded through a cross-host redirect")
	}
	if reached.Load() {
		t.Fatal("second host received the refresh request")
	}
}
