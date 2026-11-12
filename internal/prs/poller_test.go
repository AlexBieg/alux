package prs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type github struct {
	calls atomic.Int32
	fail  atomic.Bool
	delay time.Duration
}

// gql counts queue polls by their viewer request; the stack walk that follows each one finds nothing.
func (g *github) gql(_ context.Context, query string) ([]byte, error) {
	if strings.Contains(query, "pullRequests(") {
		return []byte(`{"data": {"repository": {}}}`), nil
	}
	if strings.Contains(query, " viewer {") {
		g.calls.Add(1)
	}
	time.Sleep(g.delay)
	if g.fail.Load() {
		return nil, errors.New("gh: HTTP 500")
	}
	return []byte(response), nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// session is one alux process: its own poller and clock reading, the shared cache directory.
func session(gh *github, cache *Cache, clk *clock, repo Repo) *Poller {
	p := NewPoller(Config{Poll: 4 * time.Minute, Repos: []Repo{repo}}, gh.gql, cache)
	p.now = clk.now
	return p
}

func TestSecondSessionReadsTheFirstsPoll(t *testing.T) {
	gh, clk, cache := &github{}, &clock{t: now}, &Cache{dir: t.TempDir()}
	a, b := session(gh, cache, clk, testRepo), session(gh, cache, clk, testRepo)

	a.Sync(context.Background(), false)
	clk.advance(3 * time.Minute)
	b.Sync(context.Background(), false)

	if n := gh.calls.Load(); n != 1 {
		t.Fatalf("GitHub asked %d times, want once", n)
	}
	got := b.Snapshot()
	if len(got) != 1 || len(got[0].Queue.Review) != 1 || !got[0].FetchedAt.Equal(now) {
		t.Fatalf("b adopted %+v", got)
	}
}

func TestStaleCacheIsFetchedOnceThenShared(t *testing.T) {
	gh, clk, cache := &github{}, &clock{t: now}, &Cache{dir: t.TempDir()}
	a, b := session(gh, cache, clk, testRepo), session(gh, cache, clk, testRepo)

	a.Sync(context.Background(), false)
	clk.advance(5 * time.Minute)
	b.Sync(context.Background(), false)
	a.Sync(context.Background(), false)

	if n := gh.calls.Load(); n != 2 {
		t.Fatalf("GitHub asked %d times, want twice: the first poll, then one refetch once stale", n)
	}
	if fa, fb := a.Snapshot()[0].FetchedAt, b.Snapshot()[0].FetchedAt; !fa.Equal(fb) || !fa.Equal(now.Add(5*time.Minute)) {
		t.Errorf("fetched at a=%v b=%v, want both on b's refetch", fa, fb)
	}
}

func TestConcurrentSessionsFetchOnce(t *testing.T) {
	gh, clk, cache := &github{delay: 50 * time.Millisecond}, &clock{t: now}, &Cache{dir: t.TempDir()}
	var wg sync.WaitGroup
	for range 4 {
		p := session(gh, cache, clk, testRepo)
		wg.Go(func() { p.Sync(context.Background(), false) })
	}
	wg.Wait()
	if n := gh.calls.Load(); n != 1 {
		t.Fatalf("GitHub asked %d times by four sessions at once, want once", n)
	}
}

func TestForceRefetchesAFreshCache(t *testing.T) {
	gh, clk, cache := &github{}, &clock{t: now}, &Cache{dir: t.TempDir()}
	a := session(gh, cache, clk, testRepo)
	a.Sync(context.Background(), false)
	clk.advance(time.Minute)
	a.Sync(context.Background(), true)
	if n := gh.calls.Load(); n != 2 {
		t.Fatalf("GitHub asked %d times, want a forced refetch", n)
	}
}

func TestChangedConfigIgnoresTheCache(t *testing.T) {
	gh, clk, cache := &github{}, &clock{t: now}, &Cache{dir: t.TempDir()}
	edited := testRepo
	edited.fingerprint = "edited"
	session(gh, cache, clk, testRepo).Sync(context.Background(), false)
	session(gh, cache, clk, edited).Sync(context.Background(), false)
	if n := gh.calls.Load(); n != 2 {
		t.Fatalf("GitHub asked %d times, want a refetch for the edited config", n)
	}
}

func TestFailedPollKeepsQueueAndIsNotRetriedByEverySession(t *testing.T) {
	gh, clk, cache := &github{}, &clock{t: now}, &Cache{dir: t.TempDir()}
	a, b := session(gh, cache, clk, testRepo), session(gh, cache, clk, testRepo)
	a.Sync(context.Background(), false)

	gh.fail.Store(true)
	clk.advance(5 * time.Minute)
	a.Sync(context.Background(), false)
	b.Sync(context.Background(), false)

	if n := gh.calls.Load(); n != 2 {
		t.Fatalf("GitHub asked %d times, want b to adopt a's failure rather than retry", n)
	}
	s := b.Snapshot()[0]
	if s.Err == nil || len(s.Queue.Review) != 1 || !s.FetchedAt.Equal(now) {
		t.Fatalf("b = %+v, want the old queue, its old fetch time, and the error", s)
	}
}
