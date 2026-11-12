package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
)

// Fetcher turns a URL into a cached markdown file. It is the fetch side of research: it knows
// nothing about search, and the only network it can reach is the public one.
type Fetcher struct {
	dir  string
	http *http.Client
	// allowLoopback is an unexported field, and set by no exported constructor, because httptest
	// listens on 127.0.0.1 and nothing outside a test in this package may relax the guard.
	allowLoopback bool
}

// NewFetcher caches under the directory every alux on this machine shares.
func NewFetcher() *Fetcher {
	dir := CacheDir()
	go prune(dir, cacheMaxAge, time.Now())
	return newFetcher(dir, false)
}

func newFetcher(dir string, allowLoopback bool) *Fetcher {
	f := &Fetcher{dir: dir, allowLoopback: allowLoopback}
	f.http = hardenedClient(f.guard)
	return f
}

// Fetch returns the page as markdown on disk. A cached entry is served without touching the
// network until a later fetcher's start clears it after a week, so a handle stays valid across
// the turns of a conversation; refresh is the only way to ask for the page sooner.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string, refresh bool) (Handle, error) {
	rawURL = strings.TrimSpace(rawURL)
	target, err := url.Parse(rawURL)
	if err != nil {
		return Handle{}, fmt.Errorf("%s: %w", rawURL, err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return Handle{}, fmt.Errorf("%s: only http and https pages can be fetched", rawURL)
	}

	dir := filepath.Join(f.dir, cacheKey(rawURL))
	if !refresh {
		if handle, err := load(dir, rawURL); err == nil {
			return handle, nil
		}
	}

	page, err := f.get(ctx, target)
	if err != nil {
		return Handle{}, err
	}
	if err := store(dir, page); err != nil {
		return Handle{}, fmt.Errorf("%s: %w", rawURL, err)
	}
	return handleFor(dir, page.meta, page.markdown), nil
}

type page struct {
	meta     meta
	raw      []byte
	markdown string
}

func (f *Fetcher) get(ctx context.Context, target *url.URL) (*page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	req.Header.Set("User-Agent", fetchUserAgent)
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.1")

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s: fetch failed: %s", target, resp.Status)
	}

	// The body of a format there is no extractor for is never read, least of all that of one
	// whose point is that it is not text.
	mediaType := mediaTypeOf(resp)
	if mediaType == "application/pdf" {
		return nil, fmt.Errorf("%s: %w", target, ErrPDF)
	}
	if !isHTML(mediaType) && !isText(mediaType) {
		return nil, fmt.Errorf("%s: %s: %w", target, mediaType, ErrContentType)
	}

	raw, err := readCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	final := target
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL
	}

	title, markdown := "", strings.TrimSpace(string(raw))
	if isHTML(mediaType) {
		if title, markdown, err = extract(raw, final); err != nil {
			return nil, fmt.Errorf("%s: %w", target, err)
		}
	}
	if title = strings.TrimSpace(title); title == "" {
		title = firstHeading(markdown)
	}
	if title == "" {
		title = final.String()
	}

	return &page{
		meta: meta{
			URL:         target.String(),
			FinalURL:    final.String(),
			Title:       title,
			FetchedAt:   time.Now().UTC(),
			Status:      resp.StatusCode,
			ContentType: mediaType,
		},
		raw:      raw,
		markdown: markdown,
	}, nil
}

// A page with no declared type is treated as markup: that is what an undeclared page almost
// always is, and extraction falls back to a plain conversion when it turns out not to be.
func isHTML(mediaType string) bool {
	return mediaType == "" || mediaType == "text/html" || mediaType == "application/xhtml+xml"
}

func isText(mediaType string) bool {
	switch mediaType {
	case "text/plain", "text/markdown", "text/x-markdown":
		return true
	}
	return false
}

func mediaTypeOf(resp *http.Response) string {
	parsed, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parsed))
}

func readCapped(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxPageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPageBytes {
		return nil, fmt.Errorf("page is larger than %d bytes", maxPageBytes)
	}
	return raw, nil
}

// extract drops the navigation, the sidebar and the rest of the furniture before converting, so
// what reaches the model is the article. A page readability cannot make sense of is converted
// whole rather than reported as a failure.
func extract(raw []byte, final *url.URL) (string, string, error) {
	title, source := "", string(raw)
	if article, err := readability.FromReader(bytes.NewReader(raw), final); err == nil && article.Node != nil {
		var body strings.Builder
		if article.RenderHTML(&body) == nil {
			title, source = article.Title(), body.String()
		}
	}
	markdown, err := htmltomarkdown.ConvertString(source)
	if err != nil {
		return "", "", err
	}
	return title, strings.TrimSpace(markdown), nil
}
