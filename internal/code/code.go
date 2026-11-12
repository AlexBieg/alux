// Package code draws the source the tools show: a file view, a search result, one side of a diff.
// It owns the palette and the lexers, so a fence, a diff and a view agree about what a keyword
// looks like rather than each picking its own colours.
//
// Lines are lexed one at a time, because a line is all a tool sends — a line lifted out of a block
// comment or a multi-line string is therefore coloured as if it stood alone.
package code

import (
	"image/color"
	"path/filepath"
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/x/ansi"
)

// Theme names the chroma style every code surface draws with. It is exported because glamour takes
// a theme by name for markdown fences, and naming the same one is what keeps a fenced snippet and
// a file view looking alike.
const Theme = "catppuccin-mocha"

// Wide enough to read an indent at a glance, narrow enough that deep code still fits a pane.
const TabWidth = 4

var (
	// Plain is code no lexer claimed, and the base every token colour overrides.
	Plain = lipgloss.Color("#cdd6f4")
	// Muted is for what is not code but travels with it: a line-number gutter, a bracketed note.
	Muted = lipgloss.BrightBlack
	// Landmark is for the structure around code — a path, a line number, a hunk header. Taken from
	// the theme's blue so it reads as part of the same palette.
	Landmark = lipgloss.Color("#89b4fa")
)

var theme = styles.Get(Theme)

// Line syntax highlights one line of source for path's language, over bg where bg is non-nil.
// Adjacent tokens that come out the same colour are drawn as one run: a row is re-rendered on
// every frame, and per-token escape sequences are most of its bytes.
func Line(text, path string, bg color.Color) string {
	base := lipgloss.NewStyle().Foreground(Plain)
	if bg != nil {
		base = base.Background(bg)
	}

	lexer := lexerFor(path)
	if lexer == nil || text == "" {
		return base.Render(text)
	}
	iterator, err := lexer.Tokenise(nil, text)
	if err != nil {
		return base.Render(text)
	}

	var b, run strings.Builder
	var current chroma.StyleEntry
	flush := func() {
		if run.Len() > 0 {
			b.WriteString(entryStyle(current, base).Render(run.String()))
			run.Reset()
		}
	}
	for _, token := range iterator.Tokens() {
		value := strings.TrimSuffix(token.Value, "\n")
		if value == "" {
			continue
		}
		if entry := theme.Get(token.Type); entry != current {
			flush()
			current = entry
		}
		run.WriteString(value)
	}
	flush()
	return b.String()
}

func entryStyle(entry chroma.StyleEntry, base lipgloss.Style) lipgloss.Style {
	style := base
	if entry.Colour.IsSet() {
		style = style.Foreground(lipgloss.Color(entry.Colour.String()))
	}
	if entry.Bold == chroma.Yes {
		style = style.Bold(true)
	}
	if entry.Italic == chroma.Yes {
		style = style.Italic(true)
	}
	return style
}

// Rows breaks a line into the screen rows it occupies, wrapping at width.
func Rows(text string, width int) []string {
	if text == "" {
		return []string{""}
	}
	return strings.Split(ansi.Wrap(text, width, ""), "\n")
}

// ExpandTabs replaces tabs with spaces. A tab measures zero columns, so one left in the text pads
// a row to the wrong width and puts the indent wherever the terminal's own tab stops happen to be.
func ExpandTabs(text string) string {
	return strings.ReplaceAll(text, "\t", strings.Repeat(" ", TabWidth))
}

// Lexers are matched by filename and cached: matching walks every registered lexer, and the pane
// re-renders the same handful of paths on every frame.
var (
	lexerMu    sync.Mutex
	pathLexers = map[string]chroma.Lexer{}
)

func lexerFor(path string) chroma.Lexer {
	if path == "" {
		return nil
	}
	lexerMu.Lock()
	defer lexerMu.Unlock()
	if lexer, ok := pathLexers[path]; ok {
		return lexer
	}
	lexer := lexers.Match(filepath.Base(path))
	if lexer != nil {
		lexer = chroma.Coalesce(lexer)
	}
	pathLexers[path] = lexer
	return lexer
}

// maxCached bounds a Blocks table. The table is dropped whole once it is large rather than evicted
// in order: a session's live blocks are re-rendered immediately, and the rest are dead.
const maxCached = 256

// Blocks memoizes rendered blocks, because the pane redraws every one of them on every keystroke
// and lexing a long block costs milliseconds. A zero Blocks is ready to use.
type Blocks struct {
	mu    sync.Mutex
	table map[string]string
}

// Get returns the block for key, calling build for it the first time. build runs outside the lock,
// so a slow render does not hold up another block's lookup — at the cost of two goroutines
// occasionally building the same key, which is wasted work rather than a wrong answer.
func (b *Blocks) Get(key string, build func() string) string {
	b.mu.Lock()
	out, ok := b.table[key]
	b.mu.Unlock()
	if ok {
		return out
	}

	out = build()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.table == nil || len(b.table) >= maxCached {
		b.table = map[string]string{}
	}
	b.table[key] = out
	return out
}
