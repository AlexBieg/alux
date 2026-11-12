package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	braveName     = "brave"
	braveEndpoint = "https://api.search.brave.com/res/v1/web/search"
	// Brave rejects a larger count rather than clamping it, so asking for more than this would
	// turn a request for plenty of results into no results at all.
	braveMaxCount = 20
	// An error body is read for its code, not its prose.
	braveMaxErrorBody = 4 << 10
)

// The body is consulted as well as the status because a spent plan and a momentary throttle both
// arrive as 429, and only one of them is worth waiting out.
var braveErrors = map[string]error{
	"SUBSCRIPTION_TOKEN_INVALID": ErrNoKey,
	"QUOTA_EXCEEDED":             ErrQuota,
	"PLAN_LIMIT_REACHED":         ErrQuota,
	"PLAN_EXPIRED":               ErrQuota,
}

// brave is a snippet engine: it returns descriptions and never page bodies, so every Result it
// produces leaves Text empty for the caller to fill.
type brave struct {
	key  string
	http *http.Client
	// base and now are fields only so a test can fix the endpoint and the clock.
	base string
	now  func() time.Time
}

func newBrave(cfg Config) (Provider, error) {
	if strings.TrimSpace(cfg.Key) == "" {
		return nil, fmt.Errorf("%s: %w", braveName, ErrNoKey)
	}
	return &brave{key: cfg.Key, http: cfg.client(), base: braveEndpoint}, nil
}

func (b *brave) Name() string { return braveName }

func (b *brave) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *brave) Search(ctx context.Context, q Query) ([]Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+"?"+b.params(q).Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", braveName, err)
	}
	req.Header.Set("X-Subscription-Token", b.key)
	req.Header.Set("Accept", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", braveName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, b.failure(resp)
	}

	var body struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s: %w", braveName, err)
	}

	results := make([]Result, 0, len(body.Web.Results))
	for i, hit := range body.Web.Results {
		results = append(results, Result{
			Rank:    i + 1,
			Title:   plain(hit.Title),
			URL:     hit.URL,
			Snippet: plain(hit.Description),
		})
	}
	return results, nil
}

func (b *brave) params(q Query) url.Values {
	text := strings.TrimSpace(q.Text)
	// Brave has no site parameter; the restriction goes in the query the way a user would type it.
	if site := strings.TrimSpace(q.Site); site != "" {
		text = strings.TrimSpace("site:" + site + " " + text)
	}
	v := url.Values{"q": {text}}
	if q.Count > 0 {
		v.Set("count", strconv.Itoa(min(q.Count, braveMaxCount)))
	}
	if !q.Since.IsZero() {
		v.Set("freshness", braveFreshness(q.Since, b.clock()))
	}
	return v
}

// Brave's named windows — pd, pw, pm, py — would round an arbitrary bound to one of four, so the
// explicit date range is used instead.
func braveFreshness(since, now time.Time) string {
	return since.UTC().Format(time.DateOnly) + "to" + now.UTC().Format(time.DateOnly)
}

// The body's code is read before the status, so a 429 that is really an exhausted plan does not
// come back as something to retry.
func (b *brave) failure(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, braveMaxErrorBody))
	var envelope struct {
		Error struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)

	if known := braveErrors[envelope.Error.Code]; known != nil {
		return fmt.Errorf("%s: %w", braveName, known)
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return &RateLimitedError{
			Provider:   braveName,
			RetryAfter: retryAfter(resp.Header.Get("Retry-After"), b.clock()),
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s: %w", braveName, ErrNoKey)
	case http.StatusPaymentRequired:
		return fmt.Errorf("%s: %w", braveName, ErrQuota)
	}

	detail := strings.TrimSpace(envelope.Error.Detail)
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}
	if detail == "" {
		return fmt.Errorf("%s: search failed: %s", braveName, resp.Status)
	}
	return fmt.Errorf("%s: search failed: %s: %s", braveName, resp.Status, detail)
}
