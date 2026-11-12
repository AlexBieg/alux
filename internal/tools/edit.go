package tools

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/alexbieg/alux/internal/project"
)

// fileLocks serialises each command's read-modify-write, one lock per file.
//
// Every write here reads the whole file, changes it in memory and writes it back. Two of those
// at once on one file is a lost update, and a silent one: both callers are told they succeeded
// and one edit is simply gone. Measured before this existed — two concurrent str_replace calls
// on different regions of one file lost an edit in 164 of 200 runs. A view is held too, at read
// level, because os.WriteFile truncates before it writes and a reader that overlapped one would
// see a file that is briefly half there.
//
// This makes a write atomic. It does not make two agents' intentions compatible, and only
// str_replace is anchored well enough to notice: it matches on content, so an edit whose anchor
// another run has already changed fails loudly. create overwrites wholesale and insert counts
// lines, so either can still clobber across tool calls. Different files never interact at all,
// which is the overwhelmingly common case.
//
// Keyed by cleaned absolute path, so two spellings of one file take one lock. Symlinks defeat
// that and are not handled. Entries are never removed: a mutex per path a session touches is
// nothing against the bound, and refcounting them is a deadlock waiting to be written.
var fileLocks sync.Map // cleaned absolute path -> *sync.RWMutex

func lockFor(path string) *sync.RWMutex {
	lock, _ := fileLocks.LoadOrStore(filepath.Clean(path), &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}

const (
	editMaxChars = 30_000
	// Context around a change, so a write proves itself without a second view call.
	snippetContext = 4
	// Two levels is what the tool's own description promises.
	listDepth = 2
)

// EditInput is Anthropic's text_editor_20250728 shape. The schema and the prompting come from the
// model side; this is only the executor.
//
// Note InsertText, not NewStr: str_replace uses old_str/new_str and create uses file_text, so
// new_str is the natural guess for insert and it is wrong. Getting it wrong writes a blank line
// instead of erroring, and no type checker can catch it because the input arrives as JSON.
type EditInput struct {
	Command    string `json:"command"`
	Path       string `json:"path"`
	FileText   string `json:"file_text,omitempty"`
	OldStr     string `json:"old_str,omitempty"`
	NewStr     string `json:"new_str,omitempty"`
	InsertLine *int   `json:"insert_line,omitempty"`
	InsertText string `json:"insert_text,omitempty"`
	ViewRange  []int  `json:"view_range,omitempty"`
}

// EditResult splits what the model is told from what the reader is shown. They differ for writes:
// the model only needs "ok", the transcript wants to see the change.
type EditResult struct {
	Output  string
	Display string
	IsError bool
	Image   *Image
}

func DescribeEdit(in EditInput) string {
	p := project.Short(in.Path)
	switch in.Command {
	case "view":
		if len(in.ViewRange) == 2 {
			return fmt.Sprintf("view %s:%d-%d", p, in.ViewRange[0], in.ViewRange[1])
		}
		return "view " + p
	case "create":
		return "create " + p
	case "insert":
		line := 0
		if in.InsertLine != nil {
			line = *in.InsertLine
		}
		return fmt.Sprintf("insert %s:%d", p, line)
	default:
		return "edit " + p
	}
}

// EditVerb reads back the command DescribeEdit wrote into a label, empty when the label is not
// one of its own. The renderer needs it to tell a view from a write: both arrive as the same tool
// with the same kind, and only one of them is a change.
func EditVerb(label string) string {
	verb, _, ok := strings.Cut(label, " ")
	if !ok {
		return ""
	}
	switch verb {
	case "view", "create", "insert", "edit":
		return verb
	default:
		return ""
	}
}

// EditPath reads back the path DescribeEdit wrote into a label. The renderer needs the file's
// extension to pick a syntax lexer, and the label is the only thing that reaches it.
func EditPath(label string) string {
	verb, path, ok := strings.Cut(label, " ")
	if !ok {
		return ""
	}
	switch verb {
	case "view", "create", "insert", "edit":
	default:
		return ""
	}
	// view and insert append a line or a range, which is not part of the name.
	if i := strings.LastIndex(path, ":"); i > 0 && isLineSuffix(path[i+1:]) {
		path = path[:i]
	}
	return path
}

func isLineSuffix(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func editFail(format string, args ...any) EditResult {
	msg := fmt.Sprintf(format, args...)
	return EditResult{Output: msg, Display: msg, IsError: true}
}

func truncateEdit(text string) string {
	if len(text) > editMaxChars {
		return text[:editMaxChars] + "\n[truncated]"
	}
	return text
}

// Both sides of a replacement, as the model sent them. A real diff would need a differ, and
// shelling out to `jj diff` would snapshot the working copy mid-turn, which races the agent's own
// jj commands.
func prefixLines(text, mark string) string {
	if text == "" {
		return ""
	}
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = mark + " " + line
	}
	return strings.Join(lines, "\n")
}

func diffBlock(oldStr, newStr string) string {
	var parts []string
	if s := prefixLines(oldStr, "-"); s != "" {
		parts = append(parts, s)
	}
	if s := prefixLines(newStr, "+"); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

// PreviewEdit is what the approval prompt shows: the change, from the input alone, before it is
// made. Reading the file to build a diff would race the edit being described.
func PreviewEdit(in EditInput) string {
	switch in.Command {
	case "create":
		return truncateEdit(prefixLines(in.FileText, "+"))
	case "str_replace":
		return truncateEdit(diffBlock(in.OldStr, in.NewStr))
	case "insert":
		return truncateEdit(prefixLines(in.InsertText, "+"))
	default:
		return ""
	}
}

// A file ending in a newline splits into a trailing empty element that is not a line. Counting it
// makes view report a line the file does not have and insert accept a position past the end.
func splitLines(content string) (lines []string, trailingNewline bool) {
	if content == "" {
		return nil, false
	}
	if trimmed, ok := strings.CutSuffix(content, "\n"); ok {
		return strings.Split(trimmed, "\n"), true
	}
	return strings.Split(content, "\n"), false
}

func joinLines(lines []string, trailingNewline bool) string {
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return out
}

// renderLines numbers lines from first and stops on a line boundary once budget is spent,
// reporting how many it got through. Half a line cannot be used to build a matching old_str, so
// lines come back whole — and shown is 0 when the first one did not fit even so.
func renderLines(lines []string, first, budget int) (text string, shown int) {
	width := len(fmt.Sprint(first + len(lines) - 1))
	var b strings.Builder
	for i, line := range lines {
		entry := fmt.Sprintf("%*d\t%s", width, first+i, line)
		if i > 0 {
			entry = "\n" + entry
		}
		if b.Len()+len(entry) > budget {
			if i == 0 {
				return strings.ToValidUTF8(entry[:budget], ""), 0
			}
			return b.String(), i
		}
		b.WriteString(entry)
	}
	return b.String(), len(lines)
}

// The changed region plus context, numbered, as the file now reads.
func snippetLines(lines []string, firstLine, lastLine int) string {
	start := max(1, firstLine-snippetContext)
	end := min(len(lines), lastLine+snippetContext)
	if start > end {
		return ""
	}
	text, _ := renderLines(lines[start-1:end], start, editMaxChars)
	return text
}

func RunEdit(in EditInput) EditResult {
	path := in.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(project.Cwd(), path)
	}
	// Taken around the whole command rather than inside each one, so there is a single place to
	// read the locking from and no command can be added that forgets it.
	lock := lockFor(path)
	if in.Command == "view" {
		lock.RLock()
		defer lock.RUnlock()
	} else {
		lock.Lock()
		defer lock.Unlock()
	}
	switch in.Command {
	case "view":
		return editView(path, in.ViewRange)
	case "create":
		return editCreate(path, in.FileText)
	case "str_replace":
		return editReplace(path, in.OldStr, in.NewStr)
	case "insert":
		line := 0
		if in.InsertLine != nil {
			line = *in.InsertLine
		}
		return editInsert(path, line, in.InsertText)
	default:
		return editFail("Unknown command: %s", in.Command)
	}
}

func editView(path string, viewRange []int) EditResult {
	info, err := os.Stat(path)
	if err != nil {
		return editFail("%v", err)
	}
	if info.IsDir() {
		names, err := listDir(path, listDepth)
		if err != nil {
			return editFail("%v", err)
		}
		out := truncateEdit(strings.Join(names, "\n"))
		return EditResult{Output: out, Display: out}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return editFail("%v", err)
	}
	if kind, ok := sniffImage(data); ok {
		if len(viewRange) == 2 {
			return editFail("%s is an image; view it without view_range.", project.Short(path))
		}
		img, err := loadImage(data, kind)
		if err != nil {
			return editFail("%s: %v", project.Short(path), err)
		}
		out := project.Short(path) + ": " + img.Summary()
		return EditResult{Output: out, Display: out, Image: img}
	}
	if isBinary(data) {
		return editFail("%s is a binary file (%s), not text or a supported image.",
			project.Short(path), http.DetectContentType(data))
	}
	lines, _ := splitLines(string(data))
	if len(lines) == 0 {
		out := project.Short(path) + " is empty."
		return EditResult{Output: out, Display: out}
	}
	start, end := 1, len(lines)
	if len(viewRange) == 2 {
		start = viewRange[0]
		end = viewRange[1]
		if end == -1 {
			end = len(lines)
		}
	}
	if start < 1 || start > len(lines) {
		return editFail("view_range start %d is outside 1-%d.", start, len(lines))
	}
	if end < start {
		return editFail("view_range end %d is before start %d.", end, start)
	}
	if end > len(lines) {
		end = len(lines)
	}
	out, shown := renderLines(lines[start-1:end], start, editMaxChars)
	switch last := start + shown - 1; {
	case shown == 0:
		out += fmt.Sprintf("\n[line %d is longer than %d characters and came back cut]", start, editMaxChars)
	case last < end:
		out += fmt.Sprintf("\n[clipped at line %d of %d — view_range [%d, %d] for the rest]",
			last, len(lines), last+1, end)
	}
	return EditResult{Output: out, Display: out}
}

// Hidden entries and node_modules are skipped: they dwarf what the caller is looking for.
func listDir(dir string, depth int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		if !e.IsDir() {
			names = append(names, name)
			continue
		}
		names = append(names, name+"/")
		if depth <= 1 {
			continue
		}
		children, err := listDir(filepath.Join(dir, name), depth-1)
		if err != nil {
			continue
		}
		for _, c := range children {
			names = append(names, name+"/"+c)
		}
	}
	sort.Strings(names)
	return names, nil
}

// writeNoFollow refuses a path that is a symlink when the write happens, so a link swapped in after
// the path guard looked is never followed out of where the guard allowed.
func writeNoFollow(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o644)
	if errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("%s is a symlink, and the editor does not write through links; edit its target instead", project.Short(path))
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func editCreate(path, text string) EditResult {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return editFail("%v", err)
	}
	if err := writeNoFollow(path, []byte(text)); err != nil {
		return editFail("%v", err)
	}
	lines, _ := splitLines(text)
	return EditResult{
		Output:  fmt.Sprintf("Wrote %d lines to %s.", len(lines), project.Short(path)),
		Display: truncateEdit(prefixLines(text, "+")),
	}
}

func editReplace(path, oldStr, newStr string) EditResult {
	if oldStr == "" {
		return editFail("old_str must not be empty.")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return editFail("%v", err)
	}
	content := string(data)
	first := strings.Index(content, oldStr)
	if first == -1 {
		return editFail("old_str was not found in %s. It must match the file exactly.", project.Short(path))
	}
	if strings.Contains(content[first+1:], oldStr) {
		return editFail("old_str appears more than once in %s. Include more surrounding context to make it unique.", project.Short(path))
	}
	updated := content[:first] + newStr + content[first+len(oldStr):]
	if err := writeNoFollow(path, []byte(updated)); err != nil {
		return editFail("%v", err)
	}
	lines, _ := splitLines(updated)
	firstLine := strings.Count(updated[:first], "\n") + 1
	lastLine := firstLine + strings.Count(newStr, "\n")
	return EditResult{
		Output:  fmt.Sprintf("Edited %s.\n%s", project.Short(path), snippetLines(lines, firstLine, lastLine)),
		Display: truncateEdit(diffBlock(oldStr, newStr)),
	}
}

func editInsert(path string, line int, text string) EditResult {
	// Guarding empty text is what stops the insert_text/new_str mix-up from silently splicing a
	// blank line and reporting success.
	if text == "" {
		return editFail("insert_text must not be empty.")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return editFail("%v", err)
	}
	lines, trailingNewline := splitLines(string(data))
	// An empty file has no trailing newline to preserve, but should still end with one.
	if len(lines) == 0 {
		trailingNewline = true
	}
	if line < 0 || line > len(lines) {
		return editFail("insert_line %d is outside 0-%d.", line, len(lines))
	}
	inserted := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	updated := make([]string, 0, len(lines)+len(inserted))
	updated = append(updated, lines[:line]...)
	updated = append(updated, inserted...)
	updated = append(updated, lines[line:]...)
	if err := writeNoFollow(path, []byte(joinLines(updated, trailingNewline))); err != nil {
		return editFail("%v", err)
	}
	plural := "s"
	if len(inserted) == 1 {
		plural = ""
	}
	return EditResult{
		Output: fmt.Sprintf("Inserted %d line%s into %s.\n%s", len(inserted), plural, project.Short(path),
			snippetLines(updated, line+1, line+len(inserted))),
		Display: truncateEdit(prefixLines(text, "+")),
	}
}
