package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func tempVault(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Resolved, because on macOS t.TempDir() hands back a /var path that is really /private/var
	// and every containment check here compares resolved paths.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// The census replaced an 11,109-token listing of every note path with 132 tokens of folder
// counts. What matters is that it says where things live without naming what exists — a census
// that leaked paths would quietly restore the cost it was built to remove.
func TestFolderCensusCountsWithoutListing(t *testing.T) {
	vault := tempVault(t)
	for _, rel := range []string{
		"Systems/Alpha.md", "Systems/Beta.md", "Systems/Gamma.md",
		"People/Someone.md", "HOME.md",
		".obsidian/workspace.md", // config, never a note
	} {
		full := filepath.Join(vault, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("# x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := FolderCensus(vault)
	for _, want := range []string{"Systems/ — 3 notes", "People/ — 1 notes", "./ — 1 notes"} {
		if !strings.Contains(got, want) {
			t.Errorf("census missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Alpha") || strings.Contains(got, "Someone") {
		t.Errorf("census names individual notes, which is the cost it exists to avoid:\n%s", got)
	}
	if strings.Contains(got, ".obsidian") {
		t.Errorf("census counts Obsidian's own config as notes:\n%s", got)
	}
}

// prefVault is a vault holding the preference notes, with HOME redirected so the index lands in
// a throwaway cache rather than the one live sessions share.
func prefVault(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	vault := tempVault(t)
	for rel, text := range preferenceNotes {
		writeNoteFile(t, vault, rel, text)
	}
	return vault
}

// Standing instructions come from rule records; a preference note's rule line reaching the prompt
// too would deliver every rule twice.
func TestPreferenceNotesGiveThePromptNoRules(t *testing.T) {
	got := sessionContext(prefVault(t))
	for _, rule := range []string{"Read the changelog before upgrading", "keep the original in brackets", "vault-preferences"} {
		if strings.Contains(got, rule) {
			t.Errorf("the vault note carries %q:\n%s", rule, got)
		}
	}
	if !strings.Contains(got, "Preferences/ — 3 notes") {
		t.Errorf("the rest of the session context did not render:\n%s", got)
	}
}

// No vault_note tool is registered, so a prompt that told the agent to file through it would send
// every capture at a tool that does not exist. Retrieval stays.
func TestTheVaultNoteIsReadOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	vault := tempVault(t)
	once.Do(func() {})
	saved := root
	root = vault
	t.Cleanup(func() { root = saved })

	note := SystemNote()
	if strings.Contains(note, "vault_note") {
		t.Errorf("the read-only note still mentions vault_note:\n%s", note)
	}
	for _, want := range []string{"vault_search", "vault_read", "read-only"} {
		if !strings.Contains(note, want) {
			t.Errorf("the read-only note never says %q:\n%s", want, note)
		}
	}
}

var pageNotice = regexp.MustCompile(`\n\n\[[^\n]*\]$`)
var continueAt = regexp.MustCompile(`Continue with offset=(\d+)\.\]$`)

// readAll follows each page's closing notice to the end and returns the text with the notices
// stripped, failing if the pages never end.
func readAll(t *testing.T, read func(offset int) (string, error)) (string, int) {
	t.Helper()
	var b strings.Builder
	offset, pages := 0, 0
	for ; pages < 100; pages++ {
		got, err := read(offset)
		if err != nil {
			t.Fatalf("page at offset %d: %v", offset, err)
		}
		notice := pageNotice.FindString(got)
		if notice == "" {
			t.Fatalf("page at offset %d has no closing notice:\n…%s", offset, got[max(0, len(got)-200):])
		}
		body := strings.TrimSuffix(got, notice)
		if len(body) > maxReadChars {
			t.Errorf("page at offset %d is %d chars, over the cap of %d", offset, len(body), maxReadChars)
		}
		b.WriteString(body)
		m := continueAt.FindStringSubmatch(got)
		if m == nil {
			if !strings.Contains(notice, "That is the end.") {
				t.Fatalf("last page does not say it is the end: %q", notice)
			}
			return b.String(), pages + 1
		}
		offset, _ = strconv.Atoi(m[1])
	}
	t.Fatal("pages never reached the end")
	return "", 0
}

// A note under the cap is returned untouched, with no notice to strip.
func TestAReadUnderTheCapIsUnchanged(t *testing.T) {
	short := strings.Repeat("a", maxReadChars)
	if got, err := page(short, "Systems/Short.md", 0); err != nil || got != short {
		t.Error("a note exactly at the cap was altered; the cap is inclusive")
	}
}

// Paging is only a fix if following the notices gets the reader every line, each exactly once,
// and each page ends on a line boundary so a filer can quote it back as old_text.
func TestPagesCoverAnOverlongNoteInWholeLines(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 3*maxReadChars; i++ {
		fmt.Fprintf(&b, "line %d › some prose about the scheduler — and more\n", i)
	}
	text := b.String()

	got, pages := readAll(t, func(offset int) (string, error) {
		out, err := page(text, "Systems/Long.md", offset)
		if err == nil && !strings.HasSuffix(strings.TrimSuffix(out, pageNotice.FindString(out)), "\n") {
			t.Errorf("page at offset %d ends mid-line", offset)
		}
		return out, err
	})
	if got != text {
		t.Errorf("pages joined back are %d chars, want the note's %d", len(got), len(text))
	}
	if pages < 3 {
		t.Errorf("a note three caps long came back in %d pages", pages)
	}

	lines := strings.Count(text, "\n")
	if _, err := page(text, "Systems/Long.md", lines+1); err == nil {
		t.Error("an offset past the last line was answered rather than refused")
	}
}

// A single line over the cap still has to move the reader forward, and the cut must not split a
// multi-byte character.
func TestALineOverTheCapIsCutOnARuneBoundary(t *testing.T) {
	text := "head\n" + strings.Repeat("—", maxReadChars) + "\ntail\n"
	got, err := page(text, "Systems/Wide.md", 2)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(got, pageNotice.FindString(got))
	if !utf8.ValidString(body) {
		t.Error("the cut split a multi-byte character")
	}
	if !strings.Contains(got, "was cut") || !strings.Contains(got, "offset=3") {
		t.Errorf("notice does not say the line was cut and where to continue: %q", got[len(body):])
	}
}

// The case paging exists for: a section that is itself over the cap. Pointing the reader at
// sections cannot help here, so the section has to be readable to its end by offset.
func TestAnOversizedSectionReadsToTheEnd(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 2*maxReadChars; i++ {
		fmt.Fprintf(&b, "Entry %d: the scheduler flaked again when the clocks moved.\n", i)
	}
	notes := map[string]string{
		"Systems/Scheduler Log.md": "---\ntype: system\n---\n# Scheduler Log\n\n## Incidents\n\n" +
			b.String() + "\n## Related\n\nNothing.\n",
	}
	index, vault := tempIndex(t, notes)
	refresh(t, index)

	want, err := index.ReadSection("Systems/Scheduler Log.md", "Incidents")
	if err != nil {
		t.Fatal(err)
	}
	if len(want) <= maxReadChars {
		t.Fatalf("section is %d chars, under the cap, so this asserts nothing", len(want))
	}
	got, _ := readAll(t, func(offset int) (string, error) {
		return Read(vault, "Systems/Scheduler Log.md", "Incidents", offset)
	})
	if got != want {
		t.Errorf("paged section is %d chars, want %d", len(got), len(want))
	}
}

// Search output is the model's only map of the vault now, so it has to carry the two things a
// follow-up read needs: which note, and which section within it.
func TestFormatHitsNamesTheSectionToReadNext(t *testing.T) {
	got := formatHits([]Hit{
		{Path: "Decisions/Thing.md", Heading: "Why", Snippet: "because\nof reasons"},
	})
	for _, want := range []string{"Decisions/Thing.md", "Why", "because of reasons", "section="} {
		if !strings.Contains(got, want) {
			t.Errorf("formatted hit missing %q:\n%s", want, got)
		}
	}
	// A snippet spanning lines would otherwise break the one-hit-per-pair layout.
	if strings.Contains(got, "because\nof") {
		t.Errorf("snippet newline not flattened:\n%s", got)
	}
	if formatHits(nil) != "No matches." {
		t.Errorf("empty result should say so plainly, got %q", formatHits(nil))
	}
}

// A vault holding one edge of each tier: a frontmatter declaration on the preference, and a prose
// claim in the decision about the note it says it replaces.
var supersededNotes = map[string]string{
	"Preferences/Draft Pull Requests.md": `---
type: preference
status: archived
superseded_by: "[[Stacked PRs]]"
---
# Draft Pull Requests

Every review waited on the whole branch, so this stopped being the way to open one.
`,

	"Decisions/Stacked PRs.md": `---
type: decision
status: active
---
# Stacked PRs

We moved to stacked pull requests, and this supersedes [[Triage Rota]] for
anyone triaging imports.
`,

	"Decisions/Triage Rota.md": `---
type: decision
status: active
---
# Triage Rota

Written after the queue stalled overnight.
`,
}

// Learning a note was replaced after reading it is learning too late: by then the reader has
// answered from it. The two tiers read differently because only frontmatter is authoritative.
func TestReadLeadsWithWhatSupersededTheNote(t *testing.T) {
	index, vault := tempIndex(t, supersededNotes)
	refresh(t, index)

	declared, err := Read(vault, "Preferences/Draft Pull Requests.md", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(declared, "[superseded by Decisions/Stacked PRs.md, declared in this note's frontmatter]") {
		t.Errorf("a declared supersession does not lead the read:\n%s", declared)
	}
	if !strings.Contains(declared, "Every review waited on the whole branch") {
		t.Errorf("the note itself did not come back:\n%s", declared)
	}

	detected, err := Read(vault, "Decisions/Triage Rota.md", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	notice, _, _ := strings.Cut(detected, "\n")
	for _, want := range []string{"Decisions/Stacked PRs.md", "unverified", "this supersedes [[Triage Rota]]"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the detected notice is missing %q:\n%s", want, notice)
		}
	}
	// Nothing detected is authoritative, so the one tier that is must not be imitated here.
	if strings.Contains(notice, "declared") {
		t.Errorf("a detected claim is presented as a declaration:\n%s", notice)
	}
	if !strings.Contains(detected, "Written after the queue stalled overnight") {
		t.Errorf("the note itself did not come back:\n%s", detected)
	}

	// The section route is the cheap read and the one search points at, so it cannot be the route
	// that omits the warning.
	section, err := Read(vault, "Decisions/Triage Rota.md", "Triage Rota", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(section, "[Decisions/Stacked PRs.md is recorded as superseding this note") {
		t.Errorf("a section read does not lead with the notice:\n%s", section)
	}

	// A note nothing claims to replace reads exactly as it did before.
	plain, err := Read(vault, "Decisions/Stacked PRs.md", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "---\ntype: decision") {
		t.Errorf("a note with no supersession gained a preamble:\n%s", plain)
	}
}

// The index is derived state in ~/.cache and the note is on disk, so a cache that cannot be
// opened must cost the warning and nothing else.
func TestAnUnusableIndexCostsTheSupersessionNoticeAndNotTheNote(t *testing.T) {
	index, vault := tempIndex(t, supersededNotes)
	refresh(t, index)

	// This note does carry a notice while the index works, so the absence below is the index's
	// doing rather than a fixture with nothing to warn about.
	if warned, err := Read(vault, "Preferences/Draft Pull Requests.md", "", 0); err != nil {
		t.Fatal(err)
	} else if !strings.HasPrefix(warned, "[superseded by") {
		t.Fatalf("the fixture warns about nothing, so this test asserts nothing:\n%s", warned)
	}

	// A home that is a file, not a directory, so the index's cache directory cannot be created.
	home := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(home, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	got, err := Read(vault, "Preferences/Draft Pull Requests.md", "", 0)
	if err != nil {
		t.Fatalf("a note was refused because its supersession could not be looked up: %v", err)
	}
	if !strings.HasPrefix(got, "---\ntype: preference") {
		t.Errorf("the note did not come back whole from a broken index:\n%s", got)
	}
	if strings.Contains(got, "superseded by") {
		t.Errorf("a notice was rendered from an index that could not be opened:\n%s", got)
	}
}

// The containment check is the one security boundary in this package: the subagent picks its own
// paths from model output, so a traversal or a symlink is how a filing escapes the vault.
func TestResolveRefusesPathsOutsideTheVault(t *testing.T) {
	vault := tempVault(t)

	for _, rel := range []string{
		"../escape.md",
		"Systems/../../escape.md",
		"/etc/passwd",
		".obsidian/workspace.json",
		"Systems/../.obsidian/plugins/x.md",
	} {
		if got, err := Resolve(vault, rel); err == nil {
			t.Errorf("Resolve(%q) allowed %q", rel, got)
		}
	}

	if _, err := Resolve(vault, "Systems/Foo.md"); err != nil {
		t.Errorf("a normal note path was refused: %v", err)
	}
	if _, err := Resolve(vault, "Tickets/ABC-1 Thing/ABC-1 Thing.md"); err != nil {
		t.Errorf("a nested note path was refused: %v", err)
	}
}

// A symlink inside the vault can point anywhere, so containment has to be checked after
// resolution, not on the literal path.
func TestResolveRefusesASymlinkPointingOut(t *testing.T) {
	vault := tempVault(t)
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(vault, "link.md")); err != nil {
		t.Fatal(err)
	}

	if got, err := Resolve(vault, "link.md"); err == nil {
		t.Errorf("a symlink out of the vault resolved to %q", got)
	}
}

// All or nothing. A note left half-updated by a bad edit late in the list is worse than a
// refused write, because the vault has no version control to undo it.
func TestReplaceIsAtomicAcrossEdits(t *testing.T) {
	vault := tempVault(t)
	original := "alpha\nbravo\ncharlie\n"
	note := filepath.Join(vault, "n.md")
	if err := os.WriteFile(note, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Replace(vault, "n.md", []Edit{
		{OldText: "alpha", NewText: "ALPHA"},
		{OldText: "nowhere", NewText: "x"}, // fails
	})
	if err == nil {
		t.Fatal("a replace with an unmatched edit succeeded")
	}
	if got, _ := os.ReadFile(note); string(got) != original {
		t.Errorf("the note was modified by a failed replace:\n%q", got)
	}
}

func TestReplaceRequiresAUniqueMatch(t *testing.T) {
	vault := tempVault(t)
	if err := os.WriteFile(filepath.Join(vault, "n.md"), []byte("x\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Replace(vault, "n.md", []Edit{{OldText: "x", NewText: "y"}}); err == nil {
		t.Error("an ambiguous replace succeeded")
	} else if !strings.Contains(err.Error(), "exactly once") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// The cap exists so a note is never rewritten wholesale, which is the one thing the vault's own
// rules forbid.
func TestReplaceRefusesAWholesaleRewrite(t *testing.T) {
	vault := tempVault(t)
	body := strings.Repeat("a", MaxReplaceChars+1)
	if err := os.WriteFile(filepath.Join(vault, "n.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Replace(vault, "n.md", []Edit{{OldText: body, NewText: "gone"}}); err == nil {
		t.Error("a wholesale rewrite was allowed")
	}
}

func TestCreateWillNotOverwrite(t *testing.T) {
	vault := tempVault(t)
	if _, err := Create(vault, "Systems/New.md", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(vault, "Systems/New.md", "second"); err == nil {
		t.Error("create overwrote an existing note")
	}
	if got, _ := os.ReadFile(filepath.Join(vault, "Systems/New.md")); string(got) != "first" {
		t.Errorf("note content = %q", got)
	}
	if _, err := Create(vault, "Systems/NotANote.txt", "x"); err == nil {
		t.Error("create accepted a non-markdown path")
	}
}

func TestAppendRefusesAMissingNote(t *testing.T) {
	vault := tempVault(t)
	if _, err := Append(vault, "Nope.md", "x"); err == nil {
		t.Error("append created a note that did not exist")
	}
}

// One writer at a time across every alux: two filing at once is what produces duplicate
// notes from a concurrent search-then-create.
func TestWriteLockSerialisesWriters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var mu sync.Mutex
	inside, maxInside := 0, 0
	var wg sync.WaitGroup

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = WithWriteLock(context.Background(), func() error {
				mu.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()

				time.Sleep(50 * time.Millisecond)

				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	if maxInside != 1 {
		t.Errorf("%d writers held the lock at once, want 1", maxInside)
	}
}

// A lock file whose holder cannot be read must count as held. Reading it as free is how
// several writers get in at once, because any window where the file exists without its contents
// looks identical to an abandoned lock.
//
// This is the deterministic half of the concurrency test below, which only reproduces the race
// about one run in ten and so cannot be relied on alone.
func TestUnreadableLockCountsAsHeld(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := lockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, content := range []string{"", "   ", "garbage", "1234"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if staleLock(path) {
			t.Errorf("a lock containing %q was treated as free to seize", content)
		}
	}
}

// Claiming has to be atomic: the file must never be observable without the holder written into
// it, or the window above opens on every single acquisition.
func TestClaimIsAtomic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := lockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := claim(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(string(data))) != 2 {
		t.Errorf("lock content = %q, want a pid and a timestamp", data)
	}
	if err := claim(path); err == nil {
		t.Error("claiming an already-held lock succeeded")
	}
	// No temporary files left behind to be mistaken for anything later.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("claim left %s behind", e.Name())
		}
	}
}

// An alux that dies mid-filing leaves its lock behind. The next one has to take it rather than
// waiting the full minute and then giving up.
func TestWriteLockIsTakenFromADeadHolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := lockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// PID 0 is never a live process, so this stands in for a crashed holder.
	if err := os.WriteFile(path, []byte(fmt.Sprintf("0 %d", time.Now().UnixMilli())), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		_ = WithWriteLock(context.Background(), func() error { return nil })
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a lock held by a dead process was not taken over")
	}
}

func TestWriteLockReleasesOnFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_ = WithWriteLock(context.Background(), func() error { return fmt.Errorf("boom") })

	done := make(chan struct{})
	go func() {
		_ = WithWriteLock(context.Background(), func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the lock was not released after the job failed")
	}
}
