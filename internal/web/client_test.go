package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// guarded is a fetcher with the real guard in force: nothing it is asked for in these tests may
// reach a socket.
func guarded(t *testing.T) *Fetcher {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return newFetcher(t.TempDir(), false)
}

func TestFetchRefusesAnAddressOffThePublicInternet(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"loopback", "http://127.0.0.1:9/"},
		{"IPv6 loopback", "http://[::1]:9/"},
		{"RFC1918", "http://10.0.0.1/"},
		{"RFC1918 on another range", "http://192.168.1.1/admin"},
		{"the cloud metadata address", "http://169.254.169.254/latest/meta-data/"},
		{"an IPv4-mapped private address", "http://[::ffff:10.0.0.1]/"},
		{"a unique-local IPv6 address", "http://[fd00::1]/"},
		{"unspecified", "http://0.0.0.0/"},
		{"carrier NAT", "http://100.64.0.1/"},
		{"multicast", "http://224.0.0.1/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := guarded(t)
			_, err := f.Fetch(context.Background(), tc.url, false)
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("err = %v, want %v", err, ErrBlocked)
			}
		})
	}
}

// Go hands Control the canonical form of an address a URL spelled as IPv4-mapped, so the mapped
// form is checked here, where a resolver that answered with one would land.
func TestRoutableUnmapsBeforeJudging(t *testing.T) {
	cases := map[string]bool{
		"::ffff:10.0.0.1":                    false,
		"::ffff:169.254.169.254":             false,
		"::ffff:127.0.0.1":                   false,
		"::ffff:93.184.216.34":               true,
		"2606:2800:220:1:248:1893:25c8:1946": true,
		"93.184.216.34":                      true,
	}
	for addr, want := range cases {
		if got := routable(netip.MustParseAddr(addr).Unmap()); got != want {
			t.Errorf("routable(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The guard sits on the dial and not on the URL, so the hop a redirect makes is checked the same
// way the first one was — which is the case a rebind or an open redirect would otherwise exploit.
func TestFetchRefusesARedirectToAPrivateAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	t.Setenv("HOME", t.TempDir())
	f := newFetcher(t.TempDir(), true)
	_, err := f.Fetch(context.Background(), server.URL, false)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want %v", err, ErrBlocked)
	}
}

func TestFetchStopsAfterTooManyRedirects(t *testing.T) {
	var hops atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	t.Setenv("HOME", t.TempDir())
	f := newFetcher(t.TempDir(), true)
	_, err := f.Fetch(context.Background(), server.URL, false)
	if err == nil {
		t.Fatal("an endless redirect chain came back as a page")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("err = %q, want it to name the cap", err)
	}
	if got := hops.Load(); got > maxRedirects+1 {
		t.Errorf("%d requests, want no more than %d", got, maxRedirects+1)
	}
}

func TestFetchRefusesASchemeItCannotFetch(t *testing.T) {
	f := guarded(t)
	_, err := f.Fetch(context.Background(), "file:///etc/passwd", false)
	if err == nil {
		t.Fatal("a file URL was fetched")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("err = %q, want it to say what can be fetched", err)
	}
}

// A page larger than the cap is refused rather than silently truncated: half an article cached
// as a whole one is worse than no article.
func TestFetchRefusesAPageLargerThanTheCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("a", maxPageBytes+1024)))
	}))
	t.Cleanup(server.Close)

	t.Setenv("HOME", t.TempDir())
	f := newFetcher(t.TempDir(), true)
	_, err := f.Fetch(context.Background(), server.URL, false)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want it to name the size cap", err)
	}
}
