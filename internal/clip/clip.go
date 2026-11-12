// Package clip shapes command output for the agent: bounded in size, and without the redraws a
// terminal would have consumed on its way to the screen.
package clip

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Middle drops the middle of oversized output rather than the end. A build or test run puts the
// failure summary last, so keeping only the head discards the reason it was run. The tail gets the
// larger share for the same reason.
func Middle(text string, maxChars int) string {
	return middle(text, maxChars, 0)
}

// middle is Middle for text whose middle already lost alreadyCut characters.
func middle(text string, maxChars, alreadyCut int) string {
	if len(text) <= maxChars {
		return text
	}
	head := maxChars / 3
	tail := maxChars - head
	kept := strings.TrimRight(toLineStart(text, head), "\n")
	rest := strings.TrimLeft(fromLineStart(text, len(text)-tail), "\n")
	cut := alreadyCut + utf8.RuneCountInString(text) - utf8.RuneCountInString(kept) - utf8.RuneCountInString(rest)
	return kept +
		fmt.Sprintf("\n[… %d characters cut from the middle …]\n", cut) +
		rest
}

// Both ends land on a line boundary where one is close by, which also keeps a cut from splitting
// a multi-byte rune.
func toLineStart(text string, end int) string {
	if i := strings.LastIndexByte(text[:end], '\n'); i > end-200 {
		return text[:i]
	}
	return strings.ToValidUTF8(text[:end], "")
}

func fromLineStart(text string, start int) string {
	if i := strings.IndexByte(text[start:], '\n'); i >= 0 && i < 200 {
		return text[start+i:]
	}
	return strings.ToValidUTF8(text[start:], "")
}

// CollapseCRs reduces each line to what a terminal would be showing, since a progress bar's
// earlier frames are not text anyone meant to send.
func CollapseCRs(text string) string {
	if !strings.ContainsRune(text, '\r') {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		// The return that ends a CRLF line terminates it rather than redrawing it.
		line = strings.TrimSuffix(line, "\r")
		if cr := strings.LastIndexByte(line, '\r'); cr >= 0 {
			line = line[cr+1:]
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}
