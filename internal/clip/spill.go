package clip

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The lines of a command's output shown when the agent names none: its start, and the larger share
// at the end, where a build or test run says why it failed.
const (
	DefaultHead = 50
	DefaultTail = 150
)

const (
	// maxSpill bounds the file, so a runaway command cannot fill the disk.
	maxSpill = 256 << 20
	// spillAge is how long a spill file outlives the command that wrote it.
	spillAge = 24 * time.Hour
)

// SpillDir holds the files; a variable so tests can point it elsewhere.
var SpillDir = filepath.Join(os.TempDir(), "alux-output")

// Spill keeps a command's whole output once it outgrows what the agent is shown, so a cut loses
// nothing: the notice names a file that view and grep can read.
type Spill struct {
	mu       sync.Mutex
	maxChars int

	// mem is everything written, until it passes maxChars and moves to the file.
	mem     []byte
	path    string
	file    *os.File
	filed   int64
	unsaved bool
	closed  bool

	// tail is the last maxChars bytes written, which is all a span that fits can be.
	tail     []byte
	total    int64
	lines    int64
	endsLine bool

	readAt, readLines int64
}

func NewSpill(maxChars int) *Spill { return &Spill{maxChars: maxChars, endsLine: true} }

func (s *Spill) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	s.total += int64(n)
	s.lines += int64(bytes.Count(p, []byte{'\n'}))
	s.endsLine = p[n-1] == '\n'

	s.tail = append(s.tail, p...)
	if over := len(s.tail) - s.maxChars; over > 0 {
		s.tail = s.tail[:copy(s.tail, s.tail[over:])]
	}

	switch {
	case s.file != nil:
		s.toFile(p)
	case s.path == "" && !s.unsaved:
		s.mem = append(s.mem, p...)
		if len(s.mem) > s.maxChars {
			s.persist()
		}
	}
	return n, nil
}

// Close lets go of the file once the command is done; the file itself stays for the agent to read.
func (s *Spill) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

// persist moves what is held in memory to a new file. When none can be made, only the start stays
// in memory, and the notice says the output was not saved rather than naming a file. Called under mu.
func (s *Spill) persist() {
	if s.path != "" || s.unsaved {
		return
	}
	f, err := create()
	if err != nil {
		s.unsaved = true
		s.mem = s.mem[:min(len(s.mem), s.maxChars)]
		return
	}
	s.file, s.path = f, f.Name()
	s.toFile(s.mem)
	s.mem = nil
	if s.closed {
		_ = s.file.Close()
		s.file = nil
	}
}

func create() (*os.File, error) {
	if err := os.MkdirAll(SpillDir, 0o700); err != nil {
		return nil, err
	}
	prune()
	return os.CreateTemp(SpillDir, "bash-*.log")
}

func (s *Spill) toFile(p []byte) {
	room := min(int64(len(p)), maxSpill-s.filed)
	if room <= 0 {
		return
	}
	if n, err := s.file.Write(p[:room]); err == nil {
		s.filed += int64(n)
	}
}

func prune() {
	entries, err := os.ReadDir(SpillDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > spillAge {
			_ = os.Remove(filepath.Join(SpillDir, e.Name()))
		}
	}
}

// Take is what was written since the last Take, shaped by Shape's rules.
func (s *Spill) Take(head, tail int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.shape(s.readAt, s.readLines, head, tail)
	s.readAt, s.readLines = s.total, s.lines
	return out
}

// Whole is everything written, shaped by Shape's rules, leaving Take's place alone.
func (s *Spill) Whole(head, tail int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shape(0, 0, head, tail)
}

// shape is the output from byte from (line fromLines) on: whole when it fits in head+tail lines
// and maxChars, else its first head and last tail lines around a notice naming the file. Called
// under mu.
func (s *Spill) shape(from, fromLines int64, head, tail int) string {
	span := s.total - from
	if span <= 0 {
		return ""
	}
	spanLines := s.lines - fromLines
	if !s.endsLine {
		spanLines++
	}
	if span <= int64(s.maxChars) && spanLines <= int64(head+tail) {
		return CollapseCRs(string(s.tail[int64(len(s.tail))-span:]))
	}

	s.persist()
	headText := firstLines(s.readFrom(from, s.maxChars/3), head)
	budget := min(int64(s.maxChars-len(headText)), span-int64(len(headText)))
	tailText := lastLines(s.tail[int64(len(s.tail))-min(budget, int64(len(s.tail))):], tail)

	headLines := int64(strings.Count(headText, "\n"))
	tailLines := int64(strings.Count(tailText, "\n"))
	if !s.endsLine && tailText != "" {
		tailLines++
	}
	cut := spanLines - headLines - tailLines
	first := fromLines + headLines + 1
	notice := fmt.Sprintf("[… %d lines cut, %d shown from the start and %d from the end", cut, headLines, tailLines)
	switch {
	case s.path == "":
		notice += "; the full output could not be saved …]"
	case s.total > s.filed:
		notice += fmt.Sprintf("; the first %d MB of the output are in %s, from line %d …]", s.filed>>20, s.path, first)
	default:
		notice += fmt.Sprintf("; the full output is in %s, lines %d–%d cut …]", s.path, first, first+cut-1)
	}
	return CollapseCRs(headText) + notice + "\n" + CollapseCRs(tailText)
}

// readFrom is up to n bytes of the output starting at byte from. Called under mu.
func (s *Spill) readFrom(from int64, n int) []byte {
	if s.path == "" {
		if from >= int64(len(s.mem)) {
			return nil
		}
		return s.mem[from:min(from+int64(n), int64(len(s.mem)))]
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, n)
	got, _ := f.ReadAt(buf, from)
	return buf[:got]
}

// firstLines is the first n whole lines of b, each ending in a newline; a line longer than b is
// cut at a rune boundary.
func firstLines(b []byte, n int) string {
	if n <= 0 || len(b) == 0 {
		return ""
	}
	end := 0
	for range n {
		nl := bytes.IndexByte(b[end:], '\n')
		if nl < 0 {
			break
		}
		end += nl + 1
	}
	if end == 0 {
		return strings.ToValidUTF8(string(b), "") + "\n"
	}
	return string(b[:end])
}

// lastLines is the last n lines of b, starting at a line boundary when b holds one.
func lastLines(b []byte, n int) string {
	if n <= 0 || len(b) == 0 {
		return ""
	}
	end := len(b)
	if b[end-1] == '\n' {
		end--
	}
	start := end
	for range n {
		nl := bytes.LastIndexByte(b[:start], '\n')
		if nl < 0 {
			start = -1
			break
		}
		start = nl
	}
	if start < 0 {
		// No line boundary in reach: the front of b may be the middle of a line, or of a rune.
		for len(b) > 0 && !utf8.RuneStart(b[0]) {
			b = b[1:]
		}
		return string(b)
	}
	return string(b[start+1:])
}
