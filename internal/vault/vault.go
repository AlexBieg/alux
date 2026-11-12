// Package vault gives the agent read access to the user's Obsidian vault.
//
// The vault is plain markdown on disk. The note inventory is computed once at startup: retrieval
// should never depend on the agent guessing that a note exists.
package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/taint"
)

const (
	maxSearchChars = 8_000
	maxReadChars   = 60_000
	maxRepoNotes   = 8
)

var (
	once       sync.Once
	root       string
	ticketsDir string
)

// Root is the configured vault, or "" when there is none. Resolved once.
func Root() string {
	once.Do(func() { root, ticketsDir = configured() })
	return root
}

// TicketsDir is the vault folder holding one "<KEY> <title>" folder per ticket, or "" when none is configured.
func TicketsDir() string {
	once.Do(func() { root, ticketsDir = configured() })
	return ticketsDir
}

func configured() (vault, tickets string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return "", ""
	}
	var settings struct {
		Vault      string `json:"vault"`
		TicketsDir string `json:"tickets_dir"`
	}
	if json.Unmarshal(data, &settings) != nil || settings.Vault == "" {
		return "", ""
	}
	resolved, err := filepath.EvalSymlinks(settings.Vault)
	if err != nil {
		return "", ""
	}
	return resolved, strings.Trim(settings.TicketsDir, "/")
}

// Resolve turns a vault-relative path into an absolute one, refusing anything that escapes the
// vault or reaches into Obsidian's own config. Symlinks are resolved before the check, because
// a link inside the vault can point anywhere.
func Resolve(vault, rel string) (string, error) {
	// Rejected rather than joined. filepath.Join quietly reinterprets an absolute path as
	// relative — Join(vault, "/etc/passwd") is vault/etc/passwd — so the model asking for a
	// system path would get a note created at a lookalike path inside the vault instead of an
	// error.
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the vault root")
	}
	full := filepath.Clean(filepath.Join(vault, rel))
	if !strings.HasPrefix(full, vault+string(filepath.Separator)) || strings.Contains(full, "/.obsidian/") {
		return "", fmt.Errorf("path must be inside the vault")
	}
	if real, err := filepath.EvalSymlinks(full); err == nil {
		if !strings.HasPrefix(real, vault+string(filepath.Separator)) {
			return "", fmt.Errorf("path must be inside the vault")
		}
	}
	return full, nil
}

// notePaths is the best-effort walk: a directory that cannot be read contributes nothing and is
// not reported. That is right for the prompt-side callers, where a census missing one folder
// still beats no census at all.
func notePaths(vault string) []string {
	found, _ := walkNotes(vault)
	return found
}

// notePathsChecked is the same walk, with the failures kept. The index needs the difference: a
// folder missing from the listing because it could not be read looks exactly like a folder whose
// notes were deleted, and acting on that drops rows for notes that are still on disk.
func notePathsChecked(vault string) ([]string, error) {
	return walkNotes(vault)
}

func walkNotes(vault string) ([]string, error) {
	var found []string
	var unreadable []string
	var first error
	_ = filepath.WalkDir(vault, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// d is nil when the vault root itself could not be stat'ed, so nothing below may
			// touch it.
			unreadable = append(unreadable, path)
			if first == nil {
				first = err
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			if rel, err := filepath.Rel(vault, path); err == nil {
				found = append(found, rel)
			}
		}
		return nil
	})
	sort.Strings(found)
	if first != nil {
		return found, fmt.Errorf("%d of the vault's directories could not be read (first: %s): %w",
			len(unreadable), unreadable[0], first)
	}
	return found, nil
}

// FolderCensus is the folder names and their note counts: where things live, not what exists.
//
// It replaces the full path listing, which was measured at 11,075 tokens on every request —
// roughly 90% of the system prompt, paid on turns with nothing to do with the vault at all, and
// growing with every note filed. The census is 132. The listing was there so the agent could
// see a note existed without deciding to search, and that bargain only became affordable to
// give up once search stopped failing: ranked section search answers the questions the old
// regex returned nothing for, so a guess that misses now costs one cheap call instead of a
// wrong answer.
func FolderCensus(vault string) string {
	return census(notePaths(vault))
}

func census(paths []string) string {
	counts := map[string]int{}
	var order []string
	for _, path := range paths {
		folder := "."
		if i := strings.Index(path, "/"); i >= 0 {
			folder = path[:i]
		}
		if counts[folder] == 0 {
			order = append(order, folder)
		}
		counts[folder]++
	}
	var lines []string
	for _, folder := range order {
		lines = append(lines, fmt.Sprintf("%s/ — %d notes", folder, counts[folder]))
	}
	return strings.Join(lines, "\n")
}

func run(dir, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

func currentBranch() string {
	if b := run("", "git", "branch", "--show-current"); b != "" {
		return b
	}
	b := run("", "jj", "log", "-r", "latest(::@ & bookmarks())", "--no-graph", "-T", `bookmarks.join(" ")`)
	return strings.ReplaceAll(b, "*", "")
}

var ticketKey = regexp.MustCompile(`(?i)\b[a-z]{2,8}-\d+\b`)

// The branch names the ticket, and the ticket owns a folder, so a ticketed session starts already
// knowing where that work is written.
func ticketNotes(vault, dir string) (key, folder string) {
	if dir == "" {
		return "", ""
	}
	match := ticketKey.FindString(currentBranch())
	if match == "" {
		return "", ""
	}
	key = strings.ToUpper(match)
	if name, ok := ticketFolder(vault, dir, key); ok {
		return key, dir + "/" + name
	}
	return key, ""
}

func ticketFolder(vault, dir, key string) (string, bool) {
	if dir == "" {
		return "", false
	}
	entries, err := os.ReadDir(filepath.Join(vault, dir))
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), key+" ") {
			return e.Name(), true
		}
	}
	return "", false
}

// TicketTitle is the title a ticket's folder under dir in the vault is named with, after its key.
func TicketTitle(vault, dir, key string) (string, bool) {
	if vault == "" || !ticketKey.MatchString(key) {
		return "", false
	}
	name, ok := ticketFolder(vault, dir, strings.ToUpper(key))
	if !ok {
		return "", false
	}
	title := strings.TrimSpace(strings.TrimPrefix(name, strings.ToUpper(key)+" "))
	return title, title != ""
}

// repoNotes finds notes naming this repo by path, in either form the user writes it.
func repoNotes(vault string) []string {
	repo := project.Home()
	patterns := []string{repo}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(repo, home) {
		patterns = append(patterns, "~"+strings.TrimPrefix(repo, home))
	}
	seen := map[string]bool{}
	var hits []string
	for _, pattern := range patterns {
		for _, line := range strings.Split(run(vault, "rg", "-l", "-F", "--glob", "!.obsidian", "-e", pattern, "."), "\n") {
			line = strings.TrimPrefix(line, "./")
			if line != "" && !seen[line] {
				seen[line] = true
				hits = append(hits, line)
			}
		}
	}
	sort.Strings(hits)
	if len(hits) > maxRepoNotes {
		hits = hits[:maxRepoNotes]
	}
	return hits
}

func sessionContext(vault string) string {
	sections := []string{
		"How the vault is laid out, as of when this session started. These are counts, not a " +
			"listing: to find out whether a note exists, search for it.\n<vault-folders>\n" +
			FolderCensus(vault) + "\n</vault-folders>",
	}
	if key, folder := ticketNotes(vault, TicketsDir()); key != "" {
		if folder != "" {
			sections = append(sections, fmt.Sprintf(
				"The current branch names ticket %s, whose notes are in %s/. Read its hub note in full before working on it.", key, folder))
		} else {
			sections = append(sections, fmt.Sprintf(
				"The current branch names ticket %s, which has no folder in %s/ yet.", key, TicketsDir()))
		}
	}
	if repo := repoNotes(vault); len(repo) > 0 {
		sections = append(sections, "Notes that mention this repo by path:\n- "+strings.Join(repo, "\n- "))
	}
	return strings.Join(sections, "\n\n")
}

// SystemNote is the paragraph appended to the agent's system prompt. Empty when no vault is
// configured, so the whole feature disappears rather than half-working.
func SystemNote() string {
	vault := Root()
	if vault == "" {
		return ""
	}
	return fmt.Sprintf(`The user keeps an Obsidian knowledge vault at %s: markdown notes that are their durable memory across sessions — preferences, people, systems, decisions, ticket notes, domain terms.

Use `+"`vault_search`"+` and `+"`vault_read`"+` to retrieve. Search is ranked full-text over note *sections*, so ask it a question in words rather than guessing one keyword — "why did we drop the nightly export" works, and several words that never sit on the same line together still match. It returns the heading that answers, and `+"`vault_read`"+` takes that heading as `+"`section`"+` to read just that part; omit it to read the whole note, which is what a ticket hub wants. You cannot see a list of notes, so **search before concluding the vault has nothing** — consult it whenever a request touches the user's work rather than only the code in front of you, and before asking them something they may already have told you.

%s

%s`, vault, readOnlyNote, sessionContext(vault))
}

const readOnlyNote = `The vault is read-only. Do not write, edit or create vault notes by any route, and do not offer to. Durable knowledge goes to the record store instead.`

// searchHits is how many ranked sections a search returns. Six costs roughly 1,750 tokens,
// against the 11,000 a whole-note map costs on every request.
const searchHits = 6

// Search answers from the section index, and falls back to ripgrep when the index cannot be
// built. The fallback is not a nicety: the index is derived state in ~/.cache, and a vault that
// stops being searchable because a cache file is corrupt would be a worse failure than a slow
// search. Ranked section hits beat the regex badly — five of seven realistic questions return
// nothing at all through ripgrep, because their words never appear adjacent on one line.
func Search(vault, query string) (string, error) {
	if vault == "" {
		return "", fmt.Errorf("no vault configured")
	}
	hits, err := SearchIndex(vault, query, searchHits)
	switch {
	case err == nil:
		// Per hit rather than per search: eighteen tokens of a marked note is that note's text in
		// the conversation, and a search that returned none of it has nothing to latch on.
		if slices.ContainsFunc(hits, func(h Hit) bool { return h.Sensitive }) {
			taint.Set()
		}
		return formatHits(hits), nil
	case errors.Is(err, ErrQueryUnusable):
		// The index is fine; the query is not. Falling back here would run the regex over a
		// handful of stopwords and then blame the index for it.
		return "", err
	}
	out, rgErr := regexSearch(vault, query)
	if rgErr != nil {
		// Both routes are gone. Carrying only the fallback's error loses the one that explains
		// why the index was not used, which is the answer anyone debugging this needs first.
		return "", fmt.Errorf("vault search failed: index: %w; regex fallback: %v", err, rgErr)
	}
	// Said plainly rather than silently: a session searching a degraded index should know that
	// phrasing a question is now the wrong move, because the fallback matches one regex. The
	// remedy is named because the index is derived — deleting it costs one rebuild — and nothing
	// else in the interface would tell anyone that.
	return "[section index unavailable (" + err.Error() + "); fell back to regex search, so " +
		"single keywords work and questions do not. The index is derived and safe to delete: " +
		"rm " + indexPath() + "]\n\n" + out, nil
}

func formatHits(hits []Hit) string {
	if len(hits) == 0 {
		return "No matches."
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s › %s\n    %s\n", h.Path, h.Heading,
			strings.ReplaceAll(strings.TrimSpace(h.Snippet), "\n", " "))
	}
	b.WriteString("\nRead a whole section with vault_read(path, section=\"<heading>\").")
	return b.String()
}

// regexSearch is the original ripgrep path, kept as the fallback.
func regexSearch(vault, query string) (string, error) {
	if vault == "" {
		return "", fmt.Errorf("no vault configured")
	}
	cmd := exec.Command("rg", "-i", "-n", "--max-count", "3", "--max-columns", "300", "--glob", "!.obsidian", "-e", query, ".")
	cmd.Dir = vault
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	// ripgrep exits 1 for "no matches", which is not a failure. Anything else — 2 for a bad
	// pattern, or no binary at all — is, and must not be reported as an empty vault. This is the
	// last line of defence behind a broken index, so a silent "No matches." here is the one
	// answer that could make a session conclude the vault knows nothing.
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() == 1:
	default:
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("ripgrep: %s", detail)
	}

	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = "No matches."
	}
	if len(out) > maxSearchChars {
		out = out[:maxSearchChars]
	}
	taintMatchedNotes(vault, out)
	return out, nil
}

// A ripgrep match line is `path:line:text`, relative to the vault. The path is taken as far as
// the first `.md:<digits>:`, so a filename holding a colon is read correctly and a colon in the
// matched text cannot end the path early.
var matchLineRe = regexp.MustCompile(`(?m)^(?:\./)?(.+?\.md):\d+:`)

// taintMatchedNotes latches on the notes the fallback quoted. This path has no index to ask, so
// the frontmatter is read off the files themselves — the alternative, latching whenever the
// fallback runs at all, would cost the session its egress over a corrupt cache file and a search
// that matched nothing. A quoted note that cannot be read now is treated as marked, because its
// lines are already in the answer.
//
// The scan runs over the truncated output, which is what the caller is given: a note whose lines
// were cut off contributed nothing to read.
func taintMatchedNotes(vault, out string) {
	seen := map[string]bool{}
	for _, m := range matchLineRe.FindAllStringSubmatch(out, -1) {
		rel := m[1]
		if seen[rel] {
			continue
		}
		seen[rel] = true
		full, err := Resolve(vault, rel)
		if err != nil {
			taint.Set()
			return
		}
		data, err := os.ReadFile(full)
		if err != nil || parseFrontmatter(string(data)).sensitivity != "" {
			taint.Set()
			return
		}
	}
}

// Read returns a note's text, or one section of it when section is non-empty. A section is the
// unit search returns, and it is the cheap one: across a sample of realistic queries the top hit
// cost 33 KB whole against 3 KB as the matching section. Reading whole notes is still right when
// the task is "read the hub in full", so both remain available.
//
// offset is the 1-based line to start from, as a truncation notice names it; 0 starts at the top.
func Read(vault, path, section string, offset int) (string, error) {
	if vault == "" {
		return "", fmt.Errorf("no vault configured")
	}
	if offset < 0 {
		return "", fmt.Errorf("offset %d is not a line number; lines count from 1", offset)
	}
	if section != "" {
		// Resolved first so a traversal attempt is refused on the same terms as a whole read,
		// rather than being handed to the index's own path lookup.
		if _, err := Resolve(vault, path); err != nil {
			return "", err
		}
		text, err := ReadSection(vault, path, section)
		if err != nil {
			return "", err
		}
		// Asked of the index, which is where this text came from, so the mark is exactly as fresh
		// as the words it describes. A question it cannot answer is answered the safe way.
		if marked, err := Sensitive(vault, path); err != nil || marked {
			taint.Set()
		}
		body, err := page(text, fmt.Sprintf("section %q of %s", section, path), offset)
		if err != nil {
			return "", err
		}
		return supersessionNotice(vault, path) + body, nil
	}
	full, err := Resolve(vault, path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if parseFrontmatter(string(data)).sensitivity != "" {
		taint.Set()
	}
	if len(data) == 0 {
		return "(empty note)", nil
	}
	body, err := page(string(data), path, offset)
	if err != nil {
		return "", err
	}
	return supersessionNotice(vault, path) + body, nil
}

// The notice is paid on every vault read, so the evidence is a fragment to judge the claim by.
const (
	noticeEvidenceRunes = 160
	noticeClaims        = 3
)

// supersessionNotice leads a read with what is recorded as having replaced the note. It precedes
// the text because a reader who meets it afterwards has already acted on the note, a detected
// claim is labelled unverified because only frontmatter is authoritative, and every failure is
// swallowed: a derived cache must cost the warning and never the note.
func supersessionNotice(vault, path string) string {
	x, err := OpenIndex(vault)
	if err != nil {
		return ""
	}
	defer x.Close()
	claims, err := x.SupersededBy(path)
	if err != nil || len(claims) == 0 {
		return ""
	}
	extra := 0
	if len(claims) > noticeClaims {
		extra, claims = len(claims)-noticeClaims, claims[:noticeClaims]
	}
	var b strings.Builder
	for _, claim := range claims {
		if claim.Source == sourceDeclared {
			fmt.Fprintf(&b, "[superseded by %s, declared in this note's frontmatter]\n", claim.Superseder)
			continue
		}
		// The evidence is a sentence of the claiming note's prose, so the notice carries that
		// note's marking rather than the read note's.
		if marked, err := x.Sensitive(claim.ClaimedBy); err != nil || marked {
			taint.Set()
		}
		fmt.Fprintf(&b, "[%s is recorded as superseding this note — read from prose, unverified: %q]\n",
			claim.Superseder, clipped(claim.Evidence, noticeEvidenceRunes))
	}
	if extra > 0 {
		fmt.Fprintf(&b, "[and %d further supersession claims about this note are recorded]\n", extra)
	}
	b.WriteString("\n")
	return b.String()
}

func clipped(s string, runes int) string {
	r := []rune(s)
	if len(r) <= runes {
		return s
	}
	return strings.TrimSpace(string(r[:runes])) + "…"
}

// page returns the whole lines from offset that fit under the cap, so a filer can quote them as
// old_text, and ends with where the next page starts, since a section can itself exceed the cap.
func page(text, what string, offset int) (string, error) {
	if offset <= 1 && len(text) <= maxReadChars {
		return text, nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start := max(offset, 1) - 1
	if start >= len(lines) {
		return "", fmt.Errorf("offset %d is past the end: %s has %d lines", offset, what, len(lines))
	}

	end, size := start, 0
	for end < len(lines) && size+len(lines[end]) <= maxReadChars {
		size += len(lines[end])
		end++
	}
	if end == start {
		// A single line over the cap is cut, not skipped, so the reader still moves forward.
		line, n := lines[start], maxReadChars
		for n > 0 && !utf8.RuneStart(line[n]) {
			n--
		}
		return line[:n] + fmt.Sprintf("\n\n[line %d of %s is %d chars, over the read cap of %d, and was cut. %s]",
			start+1, what, len(lines[start]), maxReadChars, nextPage(start+1, len(lines))), nil
	}

	body := strings.Join(lines[start:end], "")
	return body + fmt.Sprintf("\n\n[lines %d–%d of %d in %s. %s]",
		start+1, end, len(lines), what, nextPage(end, len(lines))), nil
}

// OffsetArg is the read tools' offset parameter, shared so the main agent's and the filer's agree.
var OffsetArg = map[string]any{
	"type":        "integer",
	"minimum":     1,
	"description": "Line to start reading from, counting from 1, as a page's closing notice names it. Omit to start at the top.",
}

func nextPage(last, total int) string {
	if last >= total {
		return "That is the end."
	}
	return fmt.Sprintf("Continue with offset=%d.", last+1)
}
