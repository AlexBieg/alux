package web

import (
	"path/filepath"
	"strings"
	"time"
)

// Handle is what the layer above holds instead of the page: enough to say what was fetched, and
// a path plus an outline to read a part of it from rather than a prompt full of markdown.
type Handle struct {
	URL       string
	FinalURL  string
	Title     string
	Path      string
	Words     int
	FetchedAt time.Time
	Outline   []Heading
}

// Heading is one markdown heading and the line it sits on in page.md, counted from 1.
type Heading struct {
	Level int
	Text  string
	Line  int
}

func handleFor(dir string, m meta, markdown string) Handle {
	return Handle{
		URL:       m.URL,
		FinalURL:  m.FinalURL,
		Title:     m.Title,
		Path:      filepath.Join(dir, pageFile),
		Words:     len(strings.Fields(markdown)),
		FetchedAt: m.FetchedAt,
		Outline:   outline(markdown),
	}
}

// outline skips fenced blocks, so a comment in a shell example is not offered as a section.
func outline(markdown string) []Heading {
	var out []Heading
	fenced := false
	for i, line := range strings.Split(markdown, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		level := len(line) - len(strings.TrimLeft(line, "#"))
		if level < 1 || level > 6 {
			continue
		}
		rest := line[level:]
		if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
			continue
		}
		if text := strings.TrimSpace(rest); text != "" {
			out = append(out, Heading{Level: level, Text: text, Line: i + 1})
		}
	}
	return out
}

func firstHeading(markdown string) string {
	if headings := outline(markdown); len(headings) > 0 {
		return headings[0].Text
	}
	return ""
}
