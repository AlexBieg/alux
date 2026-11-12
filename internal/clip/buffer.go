package clip

import (
	"sync"
	"unicode/utf8"
)

// Buffer holds only the ends of a command's output that Middle would keep, so a flood stays bounded.
type Buffer struct {
	mu       sync.Mutex
	maxChars int
	head     []byte
	headFull bool
	tail     []byte
	cut      int
}

func NewBuffer(maxChars int) *Buffer {
	return &Buffer{maxChars: maxChars}
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if !b.headFull {
		room := min(b.maxChars-len(b.head), len(p))
		if room < len(p) {
			// The head ends on a rune boundary, so the bytes it leaves start the tail cleanly.
			for room > 0 && !utf8.RuneStart(p[room]) {
				room--
			}
			b.headFull = true
		}
		b.head = append(b.head, p[:room]...)
		p = p[room:]
	}
	b.tail = append(b.tail, p...)
	if over := len(b.tail) - b.maxChars; over > 0 {
		for over < len(b.tail) && !utf8.RuneStart(b.tail[over]) {
			over++
		}
		b.cut += utf8.RuneCount(b.tail[:over])
		b.tail = b.tail[:copy(b.tail, b.tail[over:])]
	}
	return n, nil
}

// Clipped is everything written, shaped exactly as Middle would shape it, cut count included.
func (b *Buffer) Clipped() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return middle(string(b.head)+string(b.tail), b.maxChars, b.cut)
}
