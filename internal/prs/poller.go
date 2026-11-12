package prs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/reviewstore"
)

// syncEvery is how often a session rereads the shared cache; GitHub is only asked once the cache is a poll interval old.
const syncEvery = 15 * time.Second

const fetchTimeout = time.Minute

// RepoState keeps the previous queue when a poll fails, setting Err rather than going empty.
type RepoState struct {
	Repo  string
	Queue Queue
	// Stack is the open PRs stacked with a queued one that the queue does not hold.
	Stack     []PR
	Partial   []string
	FetchedAt time.Time
	Err       error
}

type Poller struct {
	conf     Config
	gql      GraphQL
	rest     REST
	send     Send
	gh       Gh
	jj       Jj
	probe    Probe
	cache    *Cache // nil polls GitHub directly, sharing nothing
	diffDir  string
	now      func() time.Time
	OnUpdate func()
	// OnTitle hears each PR title a detail or diff fetch reads.
	OnTitle func(number int, title string)
	// OnReview hears each review review_load stores.
	OnReview func(r reviewstore.Review)
	// OnPush hears the change id pr_push pushed, and returns a line to add to its result.
	OnPush func(change string) string
	// home is where review_load stores reviews; "" means the user's home directory.
	home string

	mu     sync.Mutex
	states map[string]entry
	busy   sync.Mutex
}

func NewPoller(conf Config, gql GraphQL, cache *Cache) *Poller {
	diffDir := filepath.Join(os.TempDir(), "alux-pr-diffs")
	if cache != nil {
		diffDir = filepath.Join(cache.dir, "diffs")
	}
	return &Poller{conf: conf, gql: gql, rest: GhREST, send: GhSend, gh: GhRun, jj: JjRun, probe: FFprobe(), cache: cache, diffDir: diffDir, now: time.Now, states: map[string]entry{}}
}

func (p *Poller) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(min(syncEvery, p.conf.Poll))
		defer ticker.Stop()
		for {
			p.Sync(ctx, false)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Sync reads the cache if fresh, else GitHub; force accepts only a poll made after the call began.
func (p *Poller) Sync(ctx context.Context, force bool) {
	p.busy.Lock()
	defer p.busy.Unlock()
	asked := p.now()
	changed := false
	for _, repo := range p.conf.Repos {
		if p.adopt(repo, p.syncRepo(ctx, repo, force, asked)) {
			changed = true
		}
	}
	if changed && p.OnUpdate != nil {
		p.OnUpdate()
	}
}

func (p *Poller) syncRepo(ctx context.Context, repo Repo, force bool, asked time.Time) entry {
	fresh := func(e entry) bool {
		if force {
			return !e.polledAt().Before(asked)
		}
		return p.now().Sub(e.polledAt()) < p.conf.Poll
	}
	if p.cache == nil {
		return p.poll(ctx, repo, p.current(repo))
	}
	if e, ok := p.cache.read(repo); ok && fresh(e) {
		return e
	}
	release, err := p.cache.lock(repo)
	if err != nil {
		return p.poll(ctx, repo, p.current(repo))
	}
	defer release()
	prev, ok := p.cache.read(repo)
	if ok && fresh(prev) {
		return prev
	}
	if !ok {
		prev = p.current(repo)
	}
	next := p.poll(ctx, repo, prev)
	_ = p.cache.write(repo, next)
	return next
}

// poll asks GitHub; on failure it keeps prev's queue and records why.
func (p *Poller) poll(ctx context.Context, repo Repo, prev entry) entry {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	fetched, err := Fetch(ctx, p.gql, repo)
	if err != nil {
		prev.Err, prev.ErrAt = err.Error(), p.now()
		return prev
	}
	q := Classify(fetched)
	var listed []PR
	for _, it := range append(slices.Clone(q.Review), q.Mine...) {
		listed = append(listed, it.PR)
	}
	stack, err := Stacked(ctx, p.gql, repo.Slug(), listed)
	if err != nil {
		stack = prev.Stack
	}
	return entry{FetchedAt: p.now(), Queue: q, Stack: stack, Partial: fetched.Partial}
}

func (p *Poller) current(repo Repo) entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.states[repo.Slug()]
}

func (p *Poller) adopt(repo Repo, e entry) (changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old, had := p.states[repo.Slug()]
	p.states[repo.Slug()] = e
	return !had || !old.polledAt().Equal(e.polledAt())
}

// Snapshot is every repo's state in config order, omitting repos never polled.
func (p *Poller) Snapshot() []RepoState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]RepoState, 0, len(p.conf.Repos))
	for _, repo := range p.conf.Repos {
		e, ok := p.states[repo.Slug()]
		if !ok {
			continue
		}
		s := RepoState{Repo: repo.Slug(), Queue: e.Queue, Stack: e.Stack, Partial: e.Partial, FetchedAt: e.FetchedAt}
		if e.Err != "" {
			s.Err = errors.New(e.Err)
		}
		out = append(out, s)
	}
	return out
}
