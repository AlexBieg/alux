package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

const (
	pageFile = "page.md"
	rawFile  = "raw.html"
	metaFile = "meta.json"
)

// meta is the entry's record of where the page came from. raw.html sits beside it so a later
// change of extractor can rebuild page.md without asking the site for the page again.
type meta struct {
	URL         string    `json:"url"`
	FinalURL    string    `json:"finalUrl"`
	Title       string    `json:"title"`
	FetchedAt   time.Time `json:"fetchedAt"`
	Status      int       `json:"status"`
	ContentType string    `json:"contentType"`
}

// Twelve hex characters keep the directory short enough to read in a path; the URL it stands for
// is checked on the way out, so a collision cannot serve the wrong page.
func cacheKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])[:12]
}

// cacheMaxAge is how long a fetched page is kept before a fetcher's start clears it.
const cacheMaxAge = 7 * 24 * time.Hour

// prune removes every entry in dir fetched before now minus maxAge, dating an entry without a
// readable meta.json by its directory's modification time.
func prune(dir string, maxAge time.Duration, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		var fetched time.Time
		var m meta
		if data, err := os.ReadFile(filepath.Join(path, metaFile)); err == nil && json.Unmarshal(data, &m) == nil && !m.FetchedAt.IsZero() {
			fetched = m.FetchedAt
		} else if info, err := e.Info(); err == nil {
			fetched = info.ModTime()
		} else {
			continue
		}
		if fetched.Before(cutoff) {
			os.RemoveAll(path)
		}
	}
}

// CacheDir is the directory every alux on this machine caches pages under.
func CacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, paths.Cache, "web")
}

// Origin is where a cached file came from, for a caller holding nothing but its path.
type Origin struct {
	URL       string
	FetchedAt time.Time
}

// Cached reports whether path is a file this package wrote, and what the entry it belongs to says
// it came from. Both sides are resolved before they are compared, so a path spelled with .. and a
// home that is a symlink are both answered on where they actually land.
//
// The origin is decoration and the boolean is not: an entry whose meta.json is gone or unreadable
// is still a page off the web, and is reported as one with nothing to say about it.
func Cached(path string) (Origin, bool) {
	dir, file := resolved(CacheDir()), resolved(path)
	if dir == "" || !strings.HasPrefix(file, dir+string(filepath.Separator)) {
		return Origin{}, false
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), metaFile))
	if err != nil {
		return Origin{}, true
	}
	var m meta
	if json.Unmarshal(data, &m) != nil {
		return Origin{}, true
	}
	return Origin{URL: m.URL, FetchedAt: m.FetchedAt}, true
}

// CachedPage is the markdown a fetch of rawURL left in the cache, without touching the network.
func CachedPage(rawURL string) (string, bool) {
	rawURL = strings.TrimSpace(rawURL)
	h, err := load(filepath.Join(CacheDir(), cacheKey(rawURL)), rawURL)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(h.Path)
	return string(data), err == nil
}

// resolved is the absolute path with its symlinks followed, falling back to the cleaned absolute
// path for one that is not there to resolve.
func resolved(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func load(dir, rawURL string) (Handle, error) {
	data, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return Handle{}, err
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Handle{}, fmt.Errorf("%s: %w", metaFile, err)
	}
	if m.URL != rawURL {
		return Handle{}, fmt.Errorf("%s: holds another URL", dir)
	}
	markdown, err := os.ReadFile(filepath.Join(dir, pageFile))
	if err != nil {
		return Handle{}, err
	}
	return handleFor(dir, m, string(markdown)), nil
}

// meta.json is written last: it is what load looks for first, so a directory interrupted
// half-written reads as a miss rather than as a page.
func store(dir string, p *page) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	record, err := json.MarshalIndent(p.meta, "", "  ")
	if err != nil {
		return err
	}
	files := []struct {
		name string
		body []byte
	}{
		{pageFile, []byte(p.markdown)},
		{rawFile, p.raw},
		{metaFile, record},
	}
	for _, file := range files {
		if err := writeAtomic(filepath.Join(dir, file.name), file.body); err != nil {
			return fmt.Errorf("%s: %w", file.name, err)
		}
	}
	return nil
}

// writeAtomic renames a finished temporary file over the target, so a process killed mid-write
// leaves the previous page or none, never half of one.
func writeAtomic(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
