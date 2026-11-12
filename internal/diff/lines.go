package diff

import (
	"regexp"
	"strconv"
	"strings"
)

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// NewLines is each line of a patch's new-side line number, or 0 for a header or a deleted line.
func NewLines(patch string) []int {
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	out := make([]int, len(lines))
	next := 0
	for i, l := range lines {
		if m := hunkHeader.FindStringSubmatch(l); m != nil {
			next, _ = strconv.Atoi(m[1])
			continue
		}
		if next == 0 || strings.HasPrefix(l, "-") || strings.HasPrefix(l, `\`) {
			continue
		}
		out[i] = next
		next++
	}
	return out
}
