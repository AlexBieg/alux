package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A trimmed capture of a real answer: highlight tags around the matched terms, an escaped
// entity, and the fields the package does not model sitting alongside the ones it does.
const braveBody = `{
  "query": {"original": "go generics"},
  "web": {
    "type": "search",
    "results": [
      {
        "title": "An Introduction To <strong>Generics</strong> - The <strong>Go</strong> Programming Language",
        "url": "https://go.dev/blog/intro-generics",
        "description": "<strong>Go</strong> 1.18 adds type parameters &amp; type sets to the language.",
        "page_age": "2022-03-22T00:00:00",
        "profile": {"name": "go.dev"}
      },
      {
        "title": "Type Parameters Proposal",
        "url": "https://go.googlesource.com/proposal/+/refs/heads/master/design/43651-type-parameters.md",
        "description": "This is the design doc for <strong>generics</strong>.",
        "page_age": "2021-08-20T00:00:00"
      }
    ]
  }
}`

// braveAt builds a provider pointed at a test server, with a fixed clock so the dates it sends
// and the delays it reads are exact.
func braveAt(t *testing.T, handler http.HandlerFunc) *brave {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	p, err := newBrave(Config{Key: "test-key", HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	b := p.(*brave)
	b.base = server.URL
	b.now = func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }
	return b
}

func TestBraveDecodesAnAnswer(t *testing.T) {
	b := braveAt(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Subscription-Token"); got != "test-key" {
			t.Errorf("subscription token = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("accept = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(braveBody))
	})

	results, err := b.Search(context.Background(), Query{Text: "go generics"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	first := results[0]
	if first.Rank != 1 || results[1].Rank != 2 {
		t.Errorf("ranks = %d, %d, want 1, 2", first.Rank, results[1].Rank)
	}
	if want := "An Introduction To Generics - The Go Programming Language"; first.Title != want {
		t.Errorf("title = %q, want %q", first.Title, want)
	}
	if first.URL != "https://go.dev/blog/intro-generics" {
		t.Errorf("url = %q", first.URL)
	}
	if want := "Go 1.18 adds type parameters & type sets to the language."; first.Snippet != want {
		t.Errorf("snippet = %q, want %q", first.Snippet, want)
	}
	// Brave is a snippet engine, so the caller is the one that has to fetch the page.
	for _, r := range results {
		if r.Text != "" {
			t.Errorf("rank %d came with a body: %q", r.Rank, r.Text)
		}
	}
}

func TestBraveSendsTheQueryItWasGiven(t *testing.T) {
	cases := []struct {
		name  string
		query Query
		want  url.Values
	}{
		{
			name:  "plain",
			query: Query{Text: "go generics"},
			want:  url.Values{"q": {"go generics"}},
		},
		{
			name:  "a site restriction goes into the query text",
			query: Query{Text: "generics", Site: "go.dev"},
			want:  url.Values{"q": {"site:go.dev generics"}},
		},
		{
			name:  "a bound becomes a freshness range ending today",
			query: Query{Text: "generics", Since: time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)},
			want:  url.Values{"q": {"generics"}, "freshness": {"2026-09-01to2026-10-07"}},
		},
		{
			name:  "a count above Brave's cap asks for the cap",
			query: Query{Text: "generics", Count: 50},
			want:  url.Values{"q": {"generics"}, "count": {"20"}},
		},
		{
			name:  "a count below it is passed through",
			query: Query{Text: "generics", Count: 5},
			want:  url.Values{"q": {"generics"}, "count": {"5"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got url.Values
			b := braveAt(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query()
				_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
			})
			if _, err := b.Search(context.Background(), tc.query); err != nil {
				t.Fatal(err)
			}
			if got.Encode() != tc.want.Encode() {
				t.Errorf("params = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBraveMapsFailuresOntoTypedErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, `{"type":"ErrorResponse"}`, ErrNoKey},
		{"forbidden", http.StatusForbidden, ``, ErrNoKey},
		{"an invalid token behind some other status", http.StatusBadRequest,
			`{"type":"ErrorResponse","error":{"code":"SUBSCRIPTION_TOKEN_INVALID","detail":"bad token"}}`, ErrNoKey},
		{"payment required", http.StatusPaymentRequired, ``, ErrQuota},
		{"a spent plan reported as a throttle", http.StatusTooManyRequests,
			`{"type":"ErrorResponse","error":{"code":"QUOTA_EXCEEDED","detail":"monthly quota spent"}}`, ErrQuota},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := braveAt(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := b.Search(context.Background(), Query{Text: "generics"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBraveReportsAThrottleWithItsDelay(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"seconds", "30", 30 * time.Second},
		{"an HTTP date", "Wed, 07 Oct 2026 12:02:00 GMT", 2 * time.Minute},
		{"a date already past", "Wed, 07 Oct 2026 11:00:00 GMT", 0},
		{"nothing at all", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := braveAt(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"type":"ErrorResponse","error":{"code":"RATE_LIMITED"}}`))
			})
			_, err := b.Search(context.Background(), Query{Text: "generics"})

			var limited *RateLimitedError
			if !errors.As(err, &limited) {
				t.Fatalf("err = %v, want a rate limit", err)
			}
			if limited.RetryAfter != tc.want {
				t.Errorf("retry after = %s, want %s", limited.RetryAfter, tc.want)
			}
			if limited.Provider != "brave" {
				t.Errorf("provider = %q", limited.Provider)
			}
		})
	}
}

// An unmapped failure still has to say what happened, since the caller cannot branch on it and
// the only thing left to do is report it.
func TestBraveKeepsTheDetailOfAFailureItCannotClassify(t *testing.T) {
	b := braveAt(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"ErrorResponse","error":{"code":"INTERNAL","detail":"index unavailable"}}`))
	})
	_, err := b.Search(context.Background(), Query{Text: "generics"})
	if err == nil {
		t.Fatal("a 500 came back as success")
	}
	if errors.Is(err, ErrNoKey) || errors.Is(err, ErrQuota) {
		t.Fatalf("err = %v, want nothing typed", err)
	}
	for _, want := range []string{"brave", "500", "index unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

func TestBraveHonoursACancelledContext(t *testing.T) {
	// The server never answers, so returning at all means the context did it.
	b := braveAt(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := b.Search(ctx, Query{Text: "generics"})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want it to carry the cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the search kept going after its context was cancelled")
	}
}
