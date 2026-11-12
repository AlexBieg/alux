package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneClearsPagesFetchedOverAWeekAgoAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	put := func(url string, fetched time.Time) string {
		t.Helper()
		entry := filepath.Join(dir, cacheKey(url))
		if err := store(entry, &page{meta: meta{URL: url, FetchedAt: fetched}, markdown: "# page"}); err != nil {
			t.Fatal(err)
		}
		return entry
	}
	old := put("https://example.com/old", now.Add(-8*24*time.Hour))
	fresh := put("https://example.com/fresh", now.Add(-6*24*time.Hour))
	broken := filepath.Join(dir, "brokenentry")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(broken, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	prune(dir, cacheMaxAge, now)

	for path, want := range map[string]bool{old: false, fresh: true, broken: false, stray: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s: kept = %v, want %v", filepath.Base(path), err == nil, want)
		}
	}
}

func TestCachedPageReadsWhatAFetchStored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	url := "https://example.com/a"
	p := &page{meta: meta{URL: url}, markdown: "# A\n\nSearXNG is mentioned here."}
	if err := store(filepath.Join(CacheDir(), cacheKey(url)), p); err != nil {
		t.Fatal(err)
	}
	if got, ok := CachedPage("  " + url + " "); !ok || got != p.markdown {
		t.Errorf("CachedPage = %q, %v", got, ok)
	}
	if _, ok := CachedPage("https://example.com/never-fetched"); ok {
		t.Error("a URL never fetched has no cached page")
	}
}
