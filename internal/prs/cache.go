package prs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

// cacheVersion changes whenever what is cached changes shape or meaning, so an older binary's file is refetched.
const cacheVersion = 4

// Cache shares each repo's latest poll between every alux on the machine.
type Cache struct {
	dir string
}

func DefaultCache() *Cache {
	home, _ := os.UserHomeDir()
	return &Cache{dir: filepath.Join(home, paths.Cache, "prs")}
}

type entry struct {
	Version     int       `json:"version"`
	Fingerprint string    `json:"fingerprint"`
	FetchedAt   time.Time `json:"fetched_at"`
	Queue       Queue     `json:"queue"`
	Stack       []PR      `json:"stack,omitempty"`
	Partial     []string  `json:"partial,omitempty"`
	Err         string    `json:"err,omitempty"`
	ErrAt       time.Time `json:"err_at,omitzero"`
}

// polledAt is when anyone last asked GitHub, successful or not, so a failing poll is not retried by every session at once.
func (e entry) polledAt() time.Time {
	if e.ErrAt.After(e.FetchedAt) {
		return e.ErrAt
	}
	return e.FetchedAt
}

func (c *Cache) path(repo Repo) string {
	return filepath.Join(c.dir, strings.ReplaceAll(repo.Slug(), "/", "__")+".json")
}

func (c *Cache) read(repo Repo) (entry, bool) {
	data, err := os.ReadFile(c.path(repo))
	if err != nil {
		return entry{}, false
	}
	var e entry
	if json.Unmarshal(data, &e) != nil || e.Version != cacheVersion || e.Fingerprint != repo.fingerprint {
		return entry{}, false
	}
	return e, true
}

func (c *Cache) write(repo Repo, e entry) error {
	e.Version, e.Fingerprint = cacheVersion, repo.fingerprint
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, ".prs-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path(repo))
}

// lock is held across read-fetch-write so two sessions never fetch the same repo at once; the OS drops it if the holder dies.
func (c *Cache) lock(repo Repo) (release func(), err error) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(c.path(repo)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
