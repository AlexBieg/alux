package vault

// What /vault reports, and what it can do about it.
//
// The index is derived state that nothing in the interface otherwise shows: when retrieval goes
// wrong — a search that should have matched, a note that was filed and cannot be found — there
// was no way to tell a broken index from an empty vault without opening SQLite by hand. This is
// that answer, plus the one repair worth offering.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Status is the index's health at one moment.
type Status struct {
	Vault     string
	IndexPath string
	Bytes     int64
	Schema    int
	Notes     int
	Sections  int

	// Empty is notes indexed with no sections: the vault could be walked but the file could not
	// be read. They are searchable by nothing, so a non-zero count explains a missing hit.
	Empty int

	// Searchable is sections the full-text index holds, and Aliased how many of those carry
	// generated search aliases. Reported because coverage is the one thing about aliases nothing
	// else shows: they are bought a section at a time, over minutes, and a run that stopped
	// halfway looks exactly like one that finished.
	Searchable int
	Aliased    int
	AliasPath  string
	AliasBytes int64

	// Stale and Missing are what the next search would reindex: notes whose bytes changed, and
	// notes on disk the index has never seen.
	Stale   int
	Missing int

	// Orphaned is rows whose file is gone from disk, which the next refresh would delete.
	Orphaned int

	// WalkErr is set when part of the vault could not be read. The counts above are then taken
	// over what the walk did reach, and deletion is suppressed, so Orphaned is not to be trusted.
	WalkErr error
}

func (s Status) String() string {
	label := func(k, v string) string { return fmt.Sprintf("  %-11s %s", k, v) }
	var b strings.Builder

	fmt.Fprintln(&b, label("vault", s.Vault))
	fmt.Fprintln(&b, label("index", fmt.Sprintf("%s  (%s, schema v%d)", s.IndexPath, bytesize(s.Bytes), s.Schema)))
	fmt.Fprintln(&b, label("indexed", fmt.Sprintf("%d notes, %d sections", s.Notes, s.Sections)))

	if s.Aliased == 0 {
		fmt.Fprintln(&b, label("aliases", "none — /vault aliases writes them, and they cost one model call a section"))
	} else {
		fmt.Fprintln(&b, label("aliases", fmt.Sprintf("%d of %d searchable sections  (%s, %s)",
			s.Aliased, s.Searchable, s.AliasPath, bytesize(s.AliasBytes))))
	}

	// One line that answers "will my next search see my last edit", rather than three counts the
	// reader has to add up.
	pending := s.Stale + s.Missing + s.Orphaned
	switch {
	case s.WalkErr != nil:
		fmt.Fprintln(&b, label("freshness", "cannot tell — part of the vault could not be read"))
	case pending == 0:
		fmt.Fprintln(&b, label("freshness", "up to date"))
	default:
		var parts []string
		for _, p := range []struct {
			n    int
			what string
		}{{s.Missing, "new"}, {s.Stale, "changed"}, {s.Orphaned, "deleted"}} {
			if p.n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
			}
		}
		fmt.Fprintln(&b, label("freshness", strings.Join(parts, ", ")+" — the next search picks them up"))
	}

	if s.Empty > 0 {
		fmt.Fprintln(&b, label("unreadable", fmt.Sprintf("%d notes are indexed with no text and match nothing", s.Empty)))
	}
	if s.WalkErr != nil {
		fmt.Fprintln(&b, label("warning", s.WalkErr.Error()))
	}
	return strings.TrimRight(b.String(), "\n")
}

func bytesize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Status reports on the index without changing it — deliberately: the question "is the index
// stale" must not be answered by a call that quietly makes it fresh, or it can never be
// answered yes.
func (x *Index) Status() (Status, error) {
	s := Status{Vault: x.vault, IndexPath: indexPath(), AliasPath: aliasStorePath()}
	if info, err := os.Stat(indexPath()); err == nil {
		s.Bytes = info.Size()
	}
	if info, err := os.Stat(aliasStorePath()); err == nil {
		s.AliasBytes = info.Size()
	}
	for _, q := range []struct {
		sql  string
		into *int
	}{
		{`pragma user_version`, &s.Schema},
		{`select count(*) from notes`, &s.Notes},
		{`select count(*) from sections`, &s.Sections},
		{`select count(*) from notes n where not exists (select 1 from sections where note_id = n.id)`, &s.Empty},
		{`select count(*) from sections where searchable`, &s.Searchable},
		{`select count(*) from sections where searchable and aliases <> ''`, &s.Aliased},
	} {
		if err := x.db.QueryRow(q.sql).Scan(q.into); err != nil {
			return s, err
		}
	}

	stored, err := storedNotes(x.db)
	if err != nil {
		return s, err
	}
	found, walkErr := notePathsChecked(x.vault)
	s.WalkErr = walkErr
	seen := 0
	for _, rel := range found {
		info, err := os.Stat(filepath.Join(x.vault, rel))
		if err != nil {
			continue
		}
		prev, known := stored[rel]
		switch {
		case !known:
			s.Missing++
		case prev.mtime != info.ModTime().UnixNano() || prev.size != info.Size():
			s.Stale++
		}
		seen++
	}
	s.Orphaned = len(stored) - (seen - s.Missing)
	if s.Orphaned < 0 {
		s.Orphaned = 0
	}
	return s, nil
}

// Rebuild throws the index away and builds it again from the markdown.
//
// The tables are dropped in place rather than the file being deleted: another session may have
// this database open, and unlinking the file under it would leave it reading a detached inode
// that no longer receives anyone's writes. Dropping inside a transaction makes the rebuild
// visible to every reader at once.
func (x *Index) Rebuild() (Stats, error) {
	tx, err := x.db.Begin()
	if err != nil {
		return Stats{}, err
	}
	defer tx.Rollback()
	if err := recreate(tx, x.vault); err != nil {
		return Stats{}, err
	}
	if err := tx.Commit(); err != nil {
		return Stats{}, err
	}
	return x.Refresh()
}

// VaultReport is the /vault command's body: status, or status after whatever the action asked
// for. It is here rather than in the commands package so the formatting can see the index's own
// vocabulary.
//
// An action it does not know is an error rather than a bare status, for the same reason the
// command refuses one: a mistyped repair that quietly reports the index unchanged looks exactly
// like a repair that did nothing.
func VaultReport(root, action string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("no vault is configured; set \"vault\" in ~/.config/alux/settings.json")
	}

	switch action {
	case "", "rebuild", "aliases":
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}

	var header string
	// Before the index is opened, not with it open: the run opens its own and rebuilds through
	// it, and two write connections to one file is a wait nobody needs.
	if action == "aliases" {
		start := time.Now()
		run, err := GenerateAliases(context.Background(), root, ModelGenerator(), 0)
		if err != nil {
			return "", err
		}
		header = run.AliasSummary(time.Since(start)) + "\n"
	}

	x, err := OpenIndex(root)
	if err != nil {
		return "", err
	}
	defer x.Close()

	if action == "rebuild" {
		start := time.Now()
		stats, err := x.Rebuild()
		if err != nil {
			return "", err
		}
		header = fmt.Sprintf("  Rebuilt from %d notes in %s.",
			stats.Indexed, time.Since(start).Round(time.Millisecond))
		if stats.Unreadable > 0 {
			header += fmt.Sprintf(" %d could not be read.", stats.Unreadable)
		}
		header += "\n\n"
	}

	status, err := x.Status()
	if err != nil {
		return "", err
	}
	return header + status.String(), nil
}
