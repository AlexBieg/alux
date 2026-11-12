package procs

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alexbieg/alux/internal/clip"
)

// maxBuffered is what one process may hold before its oldest output is discarded.
const maxBuffered = 64 << 10

// takeMaxChars bounds one agent-facing read, matching what a foreground command is clipped to.
const takeMaxChars = 30_000

// maxTail bounds a tail read in bytes as well as in lines, because a line is not itself bounded:
// a progress bar or a dumped file can write tens of kilobytes without a newline, and the last
// line of that is the whole buffer.
const maxTail = 2 << 10

// ring holds the tail of a process's output and tracks how much a reader has taken. Tail only,
// unlike a finished command's head-and-tail clipping: a live log's recent lines are the useful ones.
type ring struct {
	mu      sync.Mutex
	buf     []byte
	dropped int64
	read    int64
	written time.Time

	// partial is the line still being written, apart from buf so watchers see lines buf later drops.
	partial  []byte
	watchers []*lineWatch
}

// maxLine is where an unterminated line is offered to watchers anyway, so a progress bar cannot grow it forever.
const maxLine = 8 << 10

// lineWatch fires once, with the first line its pattern matches.
type lineWatch struct {
	re    *regexp.Regexp
	hit   chan string
	fired bool
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) > 0 {
		r.written = time.Now()
	}
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - maxBuffered; over > 0 {
		copy(r.buf, r.buf[over:])
		r.buf = r.buf[:maxBuffered]
		r.dropped += int64(over)
	}
	r.partial = append(r.partial, p...)
	for {
		nl := bytes.IndexByte(r.partial, '\n')
		if nl < 0 {
			break
		}
		r.offer(r.partial[:nl])
		r.partial = r.partial[nl+1:]
	}
	if len(r.partial) > maxLine {
		r.offer(r.partial)
		r.partial = nil
	}
	r.partial = append([]byte(nil), r.partial...)
	return len(p), nil
}

// offer tests one line against every watcher still waiting. Called under mu.
func (r *ring) offer(line []byte) {
	if len(r.watchers) == 0 {
		return
	}
	s := cleanLine(line)
	for _, w := range r.watchers {
		if !w.fired && w.re.MatchString(s) {
			w.fired = true
			w.hit <- s
		}
	}
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

// cleanLine is a line as a terminal would have left it: colour codes gone, carriage-return overwrites collapsed.
func cleanLine(line []byte) string {
	return strings.TrimRight(clip.CollapseCRs(ansi.ReplaceAllString(string(line), "")), "\r")
}

// watch matches re against whole lines, the untaken ones first, until the returned func stops it.
func (r *ring) watch(re *regexp.Regexp) (*lineWatch, func()) {
	w := &lineWatch{re: re, hit: make(chan string, 1)}
	r.mu.Lock()
	defer r.mu.Unlock()
	from := max(r.read-r.dropped, 0)
	unread := r.buf[from:]
	// The unterminated tail is still in partial, and is offered when it ends.
	if nl := bytes.LastIndexByte(unread, '\n'); nl >= 0 {
		for line := range bytes.SplitSeq(unread[:nl], []byte{'\n'}) {
			if re.MatchString(cleanLine(line)) {
				w.fired = true
				w.hit <- cleanLine(line)
				break
			}
		}
	}
	r.watchers = append(r.watchers, w)
	return w, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.watchers = slices.DeleteFunc(r.watchers, func(x *lineWatch) bool { return x == w })
	}
}

// finish offers the last line, which no newline ended, once the writer is done.
func (r *ring) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.partial) > 0 {
		r.offer(r.partial)
		r.partial = nil
	}
}

// take returns what was written since the last take, and how many bytes fell out of the buffer
// before the reader reached them.
func (r *ring) take() (string, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var missed int64
	from := r.read - r.dropped
	if from < 0 {
		missed, from = -from, 0
	}
	out := string(r.buf[from:])
	r.read = r.dropped + int64(len(r.buf))
	return out, missed
}

// last is when anything was last written, zero if nothing has been.
func (r *ring) last() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written
}

// whole is everything still buffered and how many bytes fell out before it, leaving the cursor alone.
func (r *ring) whole() (string, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf), r.dropped
}

// all is everything still buffered, leaving the read cursor alone.
func (r *ring) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// tail is the last n lines still buffered, leaving the read cursor alone so a watching pane takes
// no output away from the tool that reads it for the agent.
func (r *ring) tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 {
		return ""
	}
	buf := r.buf
	if len(buf) > maxTail {
		buf = buf[len(buf)-maxTail:]
		// The cap cuts at a byte, so step over whatever half-rune it left at the front.
		for len(buf) > 0 && !utf8.RuneStart(buf[0]) {
			buf = buf[1:]
		}
	}
	end := len(buf)
	// A trailing newline ends the last line rather than starting an empty one, so a tail read
	// between writes is not a blank line short.
	if end > 0 && buf[end-1] == '\n' {
		end--
	}

	start, from := 0, end
	for range n {
		nl := bytes.LastIndexByte(buf[:from], '\n')
		if nl < 0 {
			// Fewer lines buffered than were asked for, so the tail is all of it.
			start = 0
			break
		}
		start, from = nl+1, nl
	}

	return clip.CollapseCRs(string(buf[start:end]))
}
