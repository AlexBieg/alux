package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The failures a caller acts on differently: a key that will never work, a plan that is spent,
// and a throttle that will pass. Every provider maps its vendor's statuses and error codes onto
// these, so what the caller says about a failure does not change when the engine does.
var (
	ErrNoKey = errors.New("no usable API key")
	ErrQuota = errors.New("search quota exhausted")
	// The fetch side's three: a format with no extractor, a format worth naming because asking
	// for it is a reasonable mistake, and an address the fetcher will not connect to.
	ErrContentType = errors.New("unsupported content type")
	ErrPDF         = errors.New("PDFs are not supported")
	ErrBlocked     = errors.New("address is not on the public internet")
)

// RateLimitedError is the one failure worth waiting out. It names the provider because a caller
// may be running several and only one of them is being told to slow down.
type RateLimitedError struct {
	Provider string
	// Zero when the response named no delay, leaving the backoff to the caller.
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s is rate limiting, retry in %s", e.Provider, e.RetryAfter.Round(time.Second))
	}
	return e.Provider + " is rate limiting"
}

// retryAfter reads both forms RFC 9110 allows and returns zero for anything it cannot use,
// a date already past included.
func retryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
