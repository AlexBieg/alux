package clip

import (
	"strings"
	"testing"
)

// 7-byte chunks split multi-byte runes at every offset, where a mid-rune head or tail would miscount.
func TestBufferingAFloodMatchesClippingTheWholeOutput(t *testing.T) {
	const maxChars = 3_000
	var whole strings.Builder
	for i := range 20_000 {
		whole.WriteString("line é → ")
		whole.WriteString(strings.Repeat("x", i%37))
		whole.WriteByte('\n')
	}
	text := whole.String()

	b := NewBuffer(maxChars)
	peak := 0
	for p := []byte(text); len(p) > 0; {
		n := min(7, len(p))
		if _, err := b.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
		peak = max(peak, cap(b.head)+cap(b.tail))
	}

	if got, want := b.Clipped(), Middle(text, maxChars); got != want {
		t.Errorf("buffered output differs from clipping the whole of it\n got: %.200q…\nwant: %.200q…", got, want)
	}
	if peak > 4*maxChars {
		t.Errorf("buffer held %d bytes for %d of output, want it bounded near %d", peak, len(text), 2*maxChars)
	}
}
