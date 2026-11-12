package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	searxngName = "searxng"
	searxngPath = "/search"
	// The instance this was built against, and the one a self-hosted engine almost always is.
	searxngEndpoint = "http://127.0.0.1:8888"
)

// searxng is a metasearch server the user runs themselves: no key and no quota, and a snippet
// engine like the rest, so every Result it produces leaves Text empty for the caller to fill.
type searxng struct {
	http *http.Client
	// base and now are fields only so a test can fix the endpoint and the clock.
	base string
	now  func() time.Time
}

func newSearXNG(cfg Config) (Provider, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if base == "" {
		base = searxngEndpoint
	}
	return &searxng{http: cfg.client(), base: base}, nil
}

func (s *searxng) Name() string { return searxngName }

func (s *searxng) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *searxng) Search(ctx context.Context, q Query) ([]Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+searxngPath+"?"+s.params(q).Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", searxngName, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w", searxngName, err)
		}
		// A server on the user's own machine does not fail to answer for any subtler reason. Docker
		// itself is named because a container set to restart is still down whenever the daemon is.
		return nil, fmt.Errorf("%s: nothing is answering at %s; start it with `docker start alux-searxng`, "+
			"or `open -a Docker` first if the daemon is down too: %w", searxngName, s.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, s.failure(resp)
	}

	// unresponsive_engines is not read: one engine being suspended is routine and the answer the
	// others gave is still worth returning.
	var body struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s: %w", searxngName, err)
	}

	hits := body.Results
	// There is no count parameter, so the one thing left is to drop what was not asked for.
	if q.Count > 0 && len(hits) > q.Count {
		hits = hits[:q.Count]
	}
	results := make([]Result, 0, len(hits))
	for i, hit := range hits {
		results = append(results, Result{
			Rank:    i + 1,
			Title:   plain(hit.Title),
			URL:     hit.URL,
			Snippet: plain(hit.Content),
		})
	}
	return results, nil
}

func (s *searxng) params(q Query) url.Values {
	text := strings.TrimSpace(q.Text)
	// SearXNG has no site parameter; the restriction goes in the query the way a user would type it.
	if site := strings.TrimSpace(q.Site); site != "" {
		text = strings.TrimSpace("site:" + site + " " + text)
	}
	v := url.Values{"q": {text}, "format": {"json"}}
	if window := searxngWindow(q.Since, s.clock()); window != "" {
		v.Set("time_range", window)
	}
	return v
}

// The four windows cannot express a date, so a bound takes the smallest one that still contains
// it, and a bound older than all of them takes no filter rather than one that would cut results.
func searxngWindow(since, now time.Time) string {
	if since.IsZero() {
		return ""
	}
	switch age := now.Sub(since); {
	case age <= 24*time.Hour:
		return "day"
	case age <= 7*24*time.Hour:
		return "week"
	case age <= 30*24*time.Hour:
		return "month"
	case age <= 365*24*time.Hour:
		return "year"
	}
	return ""
}

func (s *searxng) failure(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return &RateLimitedError{
			Provider:   searxngName,
			RetryAfter: retryAfter(resp.Header.Get("Retry-After"), s.clock()),
		}
	case http.StatusForbidden:
		// The limiter refuses the JSON format long before it refuses the query itself.
		return fmt.Errorf("%s: the instance at %s is not serving JSON; add json to search.formats in its settings.yml",
			searxngName, s.base)
	}
	return fmt.Errorf("%s: search failed: %s", searxngName, resp.Status)
}
