// Package web is the research side of alux: one query shape, one result shape, and a
// provider per search engine behind them, alongside a fetcher that turns a URL into cached
// markdown. Nothing here knows about the agent. The vendor shows up in a provider name and
// nowhere else, so swapping engines does not change what a caller says about a result or a
// failure.
package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Query is a search expressed in what every engine can do. Anything one vendor supports and the
// others do not belongs in that vendor's own code, not here.
type Query struct {
	Text string
	// Zero takes the engine's own default; an engine capping lower than this asks for its cap.
	Count int
	Site  string
	// Zero for no bound; an engine with named windows rather than dates widens to the smallest
	// window that still contains it, so results can come back older than this.
	Since time.Time
}

// Result is one hit.
type Result struct {
	// Rank is the engine's ordering, from 1, kept because slice order survives neither a filter
	// nor a merge of two engines' answers.
	Rank    int
	Title   string
	URL     string
	Snippet string
	// Text is the page body, and only when the engine already returned one: snippet engines leave
	// it empty for the caller to fetch, content engines fill it. It sits on the result rather than
	// the provider because an engine can return a body for some of its hits and not others.
	Text string
}

// Provider is one search engine.
type Provider interface {
	Name() string
	Search(ctx context.Context, q Query) ([]Result, error)
}

// Config is everything a provider is built from. The key comes from the caller and is never read
// from a file here: this package has no business knowing where a secret lives.
type Config struct {
	Key string
	// Where a self-hosted engine answers. Empty takes that engine's own default, and a hosted
	// engine ignores it.
	URL string
	// Nil for a sensible default; supplied to share a pool or to reach a test server.
	HTTP *http.Client
}

// A search the model is waiting on is worth abandoning before the server gives up on it.
const searchTimeout = 15 * time.Second

func (c Config) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: searchTimeout, CheckRedirect: SameHostRedirects}
}

// Adding an engine is a line here plus its own file; nothing else in the package learns its name.
var providers = map[string]func(Config) (Provider, error){
	"brave":   newBrave,
	"searxng": newSearXNG,
}

// Names lists the registered providers, sorted so anything printing a choice reads the same twice.
func Names() []string {
	out := make([]string, 0, len(providers))
	for name := range providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// New builds the named provider, naming what would have worked when the name is not one.
func New(name string, cfg Config) (Provider, error) {
	build, ok := providers[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("no such search provider %q; available: %s", name, strings.Join(Names(), ", "))
	}
	return build(cfg)
}

// plain turns an engine's marked-up snippet into text: engines highlight matched terms in tags
// and escape the rest, which left in reaches the model as <strong> and &amp;.
func plain(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return strings.TrimSpace(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(html.UnescapeString(b.String()))
}
