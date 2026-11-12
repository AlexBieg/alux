package web

import (
	"strings"
	"testing"
)

func TestOutlineNamesEveryHeadingAndItsLine(t *testing.T) {
	page := strings.Join([]string{
		"# Type Parameters in Go 1.18", // 1
		"",                             // 2
		"Go 1.18 adds #hashtags to nothing at all.", // 3
		"",                                 // 4
		"## Constraints",                   // 5
		"",                                 // 6
		"```sh",                            // 7
		"# not a heading, a shell comment", // 8
		"go build ./...",                   // 9
		"```",                              // 10
		"",                                 // 11
		"### Inference",                    // 12
		"",                                 // 13
		"#no-space-is-not-a-heading",       // 14
		"####### seven is too many",        // 15
		"",                                 // 16
		"## Further reading",               // 17
	}, "\n")

	want := []Heading{
		{Level: 1, Text: "Type Parameters in Go 1.18", Line: 1},
		{Level: 2, Text: "Constraints", Line: 5},
		{Level: 3, Text: "Inference", Line: 12},
		{Level: 2, Text: "Further reading", Line: 17},
	}
	got := outline(page)
	if len(got) != len(want) {
		t.Fatalf("outline = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("heading %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The line numbers are an offer to read part of the file, so they have to index it.
	lines := strings.Split(page, "\n")
	for _, heading := range got {
		if !strings.Contains(lines[heading.Line-1], heading.Text) {
			t.Errorf("line %d is %q, not the heading %q", heading.Line, lines[heading.Line-1], heading.Text)
		}
	}
}

func TestFirstHeadingNamesAMarkdownFileWithNoTitleOfItsOwn(t *testing.T) {
	if got := firstHeading("\n# go-readability\n\nA port of Readability.js.\n"); got != "go-readability" {
		t.Errorf("first heading = %q", got)
	}
	if got := firstHeading("just prose, no headings at all\n"); got != "" {
		t.Errorf("first heading = %q, want nothing", got)
	}
}
