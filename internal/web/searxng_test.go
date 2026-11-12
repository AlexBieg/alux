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

// A trimmed capture of a real answer: the snippet under content rather than description, markup
// in both the title and the snippet, and an engine that was suspended while the rest answered.
const searxngBody = `{
  "query": "go generics",
  "number_of_results": 0,
  "results": [
    {
      "url": "https://go.dev/blog/intro-generics",
      "title": "An Introduction To <b>Generics</b> - The <b>Go</b> Programming Language",
      "content": "<b>Go</b> 1.18 adds type parameters &amp; type sets to the language.",
      "engine": "google cse",
      "score": 4.0,
      "publishedDate": null,
      "category": "general"
    },
    {
      "url": "https://go.googlesource.com/proposal/+/refs/heads/master/design/43651-type-parameters.md",
      "title": "Type Parameters Proposal",
      "content": "This is the design doc for generics.",
      "engine": "duckduckgo",
      "score": 2.0,
      "category": "general"
    },
    {
      "url": "https://pkg.go.dev/slices",
      "title": "slices package",
      "content": "Package slices defines various functions useful with slices.",
      "engine": "startpage",
      "score": 1.0,
      "category": "general"
    }
  ],
  "answers": [],
  "corrections": [],
  "infoboxes": [],
  "suggestions": [],
  "unresponsive_engines": [["brave", "Suspended: too many requests"]]
}`

// searxngAt builds a provider pointed at a test server, with a fixed clock so the window it sends
// and the delays it reads are exact.
func searxngAt(t *testing.T, handler http.HandlerFunc) *searxng {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return searxngPointedAt(t, server.URL, server.Client())
}

func searxngPointedAt(t *testing.T, base string, client *http.Client) *searxng {
	t.Helper()
	p, err := newSearXNG(Config{URL: base, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	s := p.(*searxng)
	s.now = func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }
	return s
}

// An engine of the instance's being down is routine and says nothing about the hits the others
// found, so the answer has to survive it.
func TestSearXNGDecodesAnAnswerWithASuspendedEngineInIt(t *testing.T) {
	s := searxngAt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q, want /search", r.URL.Path)
		}
		if got := r.URL.Query().Get("format"); got != "json" {
			t.Errorf("format = %q, want json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(searxngBody))
	})

	results, err := s.Search(context.Background(), Query{Text: "go generics"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}

	first := results[0]
	if first.Rank != 1 || results[1].Rank != 2 || results[2].Rank != 3 {
		t.Errorf("ranks = %d, %d, %d, want 1, 2, 3", first.Rank, results[1].Rank, results[2].Rank)
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
	// A metasearch engine returns what its engines said about a page, never the page.
	for _, r := range results {
		if r.Text != "" {
			t.Errorf("rank %d came with a body: %q", r.Rank, r.Text)
		}
	}
}

// There is no count parameter to send, so a count the caller asked for is kept by dropping the
// rest here. Asking for more than came back is not an error.
func TestSearXNGTruncatesToTheCountAsked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		want  int
	}{
		{"below what came back", 2, 2},
		{"above it", 9, 3},
		{"unset", 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got url.Values
			s := searxngAt(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query()
				_, _ = w.Write([]byte(searxngBody))
			})
			results, err := s.Search(context.Background(), Query{Text: "go generics", Count: tc.count})
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != tc.want {
				t.Errorf("got %d results, want %d", len(results), tc.want)
			}
			if got.Has("count") {
				t.Errorf("a count was sent to an engine that has no such parameter: %v", got)
			}
			if len(results) > 0 && results[len(results)-1].Rank != len(results) {
				t.Errorf("last rank = %d after truncation, want %d", results[len(results)-1].Rank, len(results))
			}
		})
	}
}

func TestSearXNGSendsTheQueryItWasGiven(t *testing.T) {
	at := func(days int) time.Time {
		return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).AddDate(0, 0, -days)
	}
	cases := []struct {
		name  string
		query Query
		want  url.Values
	}{
		{
			name:  "plain",
			query: Query{Text: "go generics"},
			want:  url.Values{"q": {"go generics"}, "format": {"json"}},
		},
		{
			name:  "a site restriction goes into the query text",
			query: Query{Text: "generics", Site: "go.dev"},
			want:  url.Values{"q": {"site:go.dev generics"}, "format": {"json"}},
		},
		{
			name:  "this morning",
			query: Query{Text: "generics", Since: at(0).Add(-6 * time.Hour)},
			want:  url.Values{"q": {"generics"}, "format": {"json"}, "time_range": {"day"}},
		},
		{
			name:  "a day exactly",
			query: Query{Text: "generics", Since: at(1)},
			want:  url.Values{"q": {"generics"}, "format": {"json"}, "time_range": {"day"}},
		},
		{
			name:  "an hour past a day widens to the week rather than cutting the day short",
			query: Query{Text: "generics", Since: at(1).Add(-time.Hour)},
			want:  url.Values{"q": {"generics"}, "format": {"json"}, "time_range": {"week"}},
		},
		{
			name:  "ten days is a month, since a week would hide nine of them",
			query: Query{Text: "generics", Since: at(10)},
			want:  url.Values{"q": {"generics"}, "format": {"json"}, "time_range": {"month"}},
		},
		{
			name:  "two months is a year",
			query: Query{Text: "generics", Since: at(60)},
			want:  url.Values{"q": {"generics"}, "format": {"json"}, "time_range": {"year"}},
		},
		{
			name:  "a bound older than every window asks for no window at all",
			query: Query{Text: "generics", Since: at(900)},
			want:  url.Values{"q": {"generics"}, "format": {"json"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got url.Values
			s := searxngAt(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query()
				_, _ = w.Write([]byte(`{"results":[]}`))
			})
			if _, err := s.Search(context.Background(), tc.query); err != nil {
				t.Fatal(err)
			}
			if got.Encode() != tc.want.Encode() {
				t.Errorf("params = %v, want %v", got, tc.want)
			}
		})
	}
}

// The instance is on the user's own machine, so the likeliest failure by far is that it is not
// running, and the message is the whole of the fix.
func TestSearXNGSaysHowToStartAnInstanceThatIsNotAnswering(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	client := server.Client()
	base := server.URL
	server.Close()

	s := searxngPointedAt(t, base, client)
	_, err := s.Search(context.Background(), Query{Text: "generics"})
	if err == nil {
		t.Fatal("a closed port came back as success")
	}
	for _, want := range []string{"searxng", base, "docker start alux-searxng", "open -a Docker"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

func TestSearXNGMapsFailuresOntoTypedErrors(t *testing.T) {
	t.Run("a throttle is worth waiting out", func(t *testing.T) {
		s := searxngAt(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		_, err := s.Search(context.Background(), Query{Text: "generics"})

		var limited *RateLimitedError
		if !errors.As(err, &limited) {
			t.Fatalf("err = %v, want a rate limit", err)
		}
		if limited.Provider != "searxng" || limited.RetryAfter != 30*time.Second {
			t.Errorf("limit = %+v, want searxng and 30s", limited)
		}
	})

	t.Run("a refusal is the instance not serving JSON", func(t *testing.T) {
		s := searxngAt(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		})
		_, err := s.Search(context.Background(), Query{Text: "generics"})
		if err == nil {
			t.Fatal("a 403 came back as success")
		}
		// A keyless engine has no key to blame, so the one thing left is the setting that did it.
		if errors.Is(err, ErrNoKey) {
			t.Errorf("err = %v, want nothing about a key", err)
		}
		for _, want := range []string{"json", "search.formats", "settings.yml"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to mention %q", err, want)
			}
		}
	})

	t.Run("anything else still says what happened", func(t *testing.T) {
		s := searxngAt(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := s.Search(context.Background(), Query{Text: "generics"})
		if err == nil {
			t.Fatal("a 500 came back as success")
		}
		for _, want := range []string{"searxng", "500"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to mention %q", err, want)
			}
		}
	})
}

func TestSearXNGHonoursACancelledContext(t *testing.T) {
	// The server never answers, so returning at all means the context did it.
	s := searxngAt(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := s.Search(ctx, Query{Text: "generics"})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want it to carry the cancellation", err)
		}
		// The instance answered every other request; telling the user to start it would be a lie.
		if strings.Contains(err.Error(), "docker start") {
			t.Errorf("err = %q, want a cancellation reported as one", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the search kept going after its context was cancelled")
	}
}

func TestSearXNGDefaultsToTheLocalInstance(t *testing.T) {
	p, err := newSearXNG(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if base := p.(*searxng).base; base != "http://127.0.0.1:8888" {
		t.Errorf("base = %q, want the local instance", base)
	}
}
