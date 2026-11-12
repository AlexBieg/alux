package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

// A page shaped like a real one: a banner, a navigation bar, a sidebar of related links and a
// footer around the thing a reader came for.
const articleHTML = `<!DOCTYPE html>
<html lang="en">
<head><title>Type Parameters in Go 1.18 | The Go Blog</title></head>
<body>
<header id="banner"><a href="/">The Go Blog</a><form><input name="q"><button>Search</button></form></header>
<nav class="site-nav"><ul><li><a href="/doc">Documentation</a></li><li><a href="/pkg">Packages</a></li><li><a href="/blog">Blog</a></li><li><a href="/help">Help</a></li></ul></nav>
<div class="layout">
<article>
<h1>Type Parameters in Go 1.18</h1>
<p>Go 1.18 adds support for type parameters, the largest change to the language since its first release. A function or a type may now be written once and used with any of the types that satisfy the constraint it declares, which removes a long-standing reason to reach for reflection or for code generation when writing a container or an algorithm that does not care what it holds.</p>
<h2>Constraints</h2>
<p>A constraint is an interface, and an interface may now embed a set of types as well as a set of methods. The compiler checks at the call site that the type argument is a member of the set, so a mistake is reported where the mistake was made rather than deep inside the function that was instantiated with it. Constraints are ordinary interfaces, which means they compose the way interfaces always have.</p>
<h2>Inference</h2>
<p>Type arguments can usually be left out. The compiler infers them from the ordinary arguments to the call, falling back to the constraints when the arguments alone are not enough to pin a parameter down. Inference is deliberately limited: a rule that solves every case would also be a rule that nobody could predict the behaviour of while reading a call.</p>
</article>
<aside class="sidebar"><h3>Related posts</h3><ul><li><a href="/blog/go1.17">Go 1.17 is released</a></li><li><a href="/blog/why-generics">Why generics?</a></li></ul><div class="promo">Subscribe to our newsletter for weekly updates!</div></aside>
</div>
<footer><p>Copyright 2026 the Go Authors. All rights reserved. Terms of service and privacy policy.</p></footer>
</body>
</html>`

// serve runs a handler on loopback and returns a fetcher whose cache is a throwaway directory.
// The guard stays on for every address but the one httptest listens on.
func serve(t *testing.T, handler http.HandlerFunc) (*Fetcher, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("HOME", t.TempDir())
	return newFetcher(t.TempDir(), true), server.URL
}

func markup(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}
}

func TestFetchKeepsTheArticleAndDropsTheChrome(t *testing.T) {
	f, url := serve(t, markup(articleHTML))
	handle, err := f.Fetch(context.Background(), url, false)
	if err != nil {
		t.Fatal(err)
	}

	if want := "Type Parameters in Go 1.18"; !strings.Contains(handle.Title, want) {
		t.Errorf("title = %q, want it to contain %q", handle.Title, want)
	}
	markdown, err := os.ReadFile(handle.Path)
	if err != nil {
		t.Fatal(err)
	}
	page := string(markdown)
	for _, want := range []string{
		"## Constraints",
		"## Inference",
		"the largest change to the language",
		"a rule that nobody could predict",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page.md lost %q:\n%s", want, page)
		}
	}
	for _, unwanted := range []string{"Subscribe to our newsletter", "Related posts", "Documentation", "privacy policy"} {
		if strings.Contains(page, unwanted) {
			t.Errorf("page.md kept the chrome %q:\n%s", unwanted, page)
		}
	}
	if handle.Words < 100 {
		t.Errorf("words = %d, want the article's worth", handle.Words)
	}
	if len(handle.Outline) < 2 {
		t.Errorf("outline = %v, want the article's headings", handle.Outline)
	}
}

func TestFetchRoutesOnContentType(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantErr     error
		wantText    string
		wantTitle   string
	}{
		{
			name:        "markup is extracted",
			contentType: "text/html; charset=utf-8",
			body:        articleHTML,
			wantText:    "## Constraints",
			wantTitle:   "Type Parameters in Go 1.18",
		},
		{
			name:        "plain text is passed through",
			contentType: "text/plain; charset=utf-8",
			body:        "RFC 9110 defines the semantics of HTTP.\n\n  indented, <not> markup\n",
			wantText:    "  indented, <not> markup",
		},
		{
			name:        "markdown is passed through and names itself",
			contentType: "text/markdown",
			body:        "# go-readability\n\nA port of Readability.js.\n",
			wantText:    "# go-readability",
			wantTitle:   "go-readability",
		},
		{
			name:        "a PDF is refused by name",
			contentType: "application/pdf",
			body:        "%PDF-1.7\n1 0 obj\n\x00\x01\x02binary",
			wantErr:     ErrPDF,
		},
		{
			name:        "anything else is refused as unsupported",
			contentType: "image/png",
			body:        "\x89PNG\r\n\x1a\n",
			wantErr:     ErrContentType,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, url := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.body))
			})
			handle, err := f.Fetch(context.Background(), url, false)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if entries, _ := os.ReadDir(f.dir); len(entries) != 0 {
					t.Errorf("a refused page left %d cache entries", len(entries))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			page, err := os.ReadFile(handle.Path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(page), tc.wantText) {
				t.Errorf("page.md = %q, want it to contain %q", page, tc.wantText)
			}
			if tc.wantTitle != "" && !strings.Contains(handle.Title, tc.wantTitle) {
				t.Errorf("title = %q, want it to contain %q", handle.Title, tc.wantTitle)
			}
			if tc.wantTitle == "" && handle.Title != url {
				t.Errorf("title = %q, want the URL where the page has no title of its own", handle.Title)
			}
		})
	}
}

func TestFetchReportsAFailedStatus(t *testing.T) {
	f, url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html><body><h1>Not found</h1></body></html>"))
	})
	_, err := f.Fetch(context.Background(), url, false)
	if err == nil {
		t.Fatal("a 404 came back as a page")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %q, want it to name the status", err)
	}
	if entries, _ := os.ReadDir(f.dir); len(entries) != 0 {
		t.Errorf("a 404 was cached as a page")
	}
}

func TestFetchServesTheSecondAskFromDisk(t *testing.T) {
	var requests atomic.Int64
	f, url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		markup(articleHTML)(w, r)
	})

	first, err := f.Fetch(context.Background(), url, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Fetch(context.Background(), url, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("%d requests, want the second ask to be served from disk", got)
	}
	if second.Path != first.Path || second.Title != first.Title || !second.FetchedAt.Equal(first.FetchedAt) {
		t.Errorf("cached handle = %+v, want %+v", second, first)
	}
	if second.Words != first.Words || len(second.Outline) != len(first.Outline) {
		t.Errorf("cached outline = %+v, want %+v", second.Outline, first.Outline)
	}

	if _, err := f.Fetch(context.Background(), url, true); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("%d requests, want refresh to have asked again", got)
	}
}

func TestFetchWritesTheEntryWholeAndLeavesNoTempFiles(t *testing.T) {
	f, url := serve(t, markup(articleHTML))
	handle, err := f.Fetch(context.Background(), url, false)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(handle.Path)
	if want := filepath.Join(f.dir, cacheKey(url)); dir != want {
		t.Errorf("entry at %s, want %s", dir, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		if strings.HasPrefix(entry.Name(), ".") {
			t.Errorf("a temporary file was left behind: %s", entry.Name())
		}
	}
	if len(names) != 3 {
		t.Fatalf("entry holds %v, want exactly page.md, raw.html and meta.json", names)
	}

	raw, err := os.ReadFile(filepath.Join(dir, rawFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != articleHTML {
		t.Errorf("raw.html is not what the server sent")
	}
	record, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		t.Fatal(err)
	}
	var m meta
	if err := json.Unmarshal(record, &m); err != nil {
		t.Fatal(err)
	}
	if m.URL != url || m.FinalURL != url {
		t.Errorf("meta urls = %q, %q, want %q", m.URL, m.FinalURL, url)
	}
	if m.Status != http.StatusOK || m.ContentType != "text/html" {
		t.Errorf("meta status/type = %d, %q", m.Status, m.ContentType)
	}
	if m.Title != handle.Title || !m.FetchedAt.Equal(handle.FetchedAt) {
		t.Errorf("meta = %+v, want it to agree with the handle %+v", m, handle)
	}
	if time.Since(m.FetchedAt) > time.Minute {
		t.Errorf("fetched at %s, want about now", m.FetchedAt)
	}
}

// A redirect is followed, and the handle says where the page actually came from.
func TestFetchRecordsTheURLItEndedAt(t *testing.T) {
	f, url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/final" {
			http.Redirect(w, r, "/final", http.StatusMovedPermanently)
			return
		}
		markup(articleHTML)(w, r)
	})
	handle, err := f.Fetch(context.Background(), url, false)
	if err != nil {
		t.Fatal(err)
	}
	if handle.URL != url {
		t.Errorf("url = %q, want the one that was asked for, %q", handle.URL, url)
	}
	if want := url + "/final"; handle.FinalURL != want {
		t.Errorf("final url = %q, want %q", handle.FinalURL, want)
	}
}

func TestFetchHonoursACancelledContext(t *testing.T) {
	// The server never answers, so returning at all means the context did it.
	f, url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := f.Fetch(ctx, url, false)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want it to carry the cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fetch kept going after its context was cancelled")
	}
}

func TestNewFetcherCachesWhereTheOtherCachesLive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := NewFetcher().dir, filepath.Join(home, paths.Cache, "web"); got != want {
		t.Errorf("cache dir = %q, want %q", got, want)
	}
}
