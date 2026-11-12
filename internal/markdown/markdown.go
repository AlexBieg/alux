// Package markdown renders the model's prose for the pane.
//
// This is glamour with a few options set; it wraps table cells to the render width itself, so a
// wide table never pushes past the pane. Table body rows are divided by rules, which glamour
// cannot do on its own — see tablerules.go. Mermaid fences are drawn as diagrams — see mermaid.go.
package markdown

import (
	"strings"
	"sync"

	"charm.land/glamour/v2"
	glamourAnsi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/alexbieg/alux/internal/code"
	"github.com/charmbracelet/x/ansi"
)

const (
	defaultWidth = 80
	// MinWidth is the narrowest width Render lays out at; anything narrower gets defaultWidth.
	MinWidth = 20
)

// Renderers are cached per width: building one parses a style config, and the pane only has a
// handful of widths over a session.
var (
	mu        sync.Mutex
	renderers = map[int]*glamour.TermRenderer{}
)

func rendererFor(width int) *glamour.TermRenderer {
	mu.Lock()
	defer mu.Unlock()
	if r, ok := renderers[width]; ok {
		return r
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(chatStyle(width)),
		glamour.WithWordWrap(width),
		glamour.WithTableWrap(true),
		glamour.WithEmoji(),
		// glamour defaults to terminal256, which rounds the theme's colours to the xterm cube and
		// so disagrees with the tools, which draw the same palette in truecolour.
		glamour.WithChromaFormatter("terminal16m"),
	)
	if err != nil {
		return nil
	}
	renderers[width] = r
	return r
}

// The prose hues, taken from catppuccin-mocha — the same ramp internal/code draws source with, so
// a heading and a keyword are picked from one palette rather than two.
const (
	headingBlue   = "#89b4fa" // h1, and code.Landmark
	headingGreen  = "#a6e3a1" // h2
	headingPeach  = "#fab387" // h3
	headingMauve  = "#cba6f7" // h4
	headingTeal   = "#94e2d5" // h5
	headingGrey   = "#a6adc8" // h6
	markerGreen   = "#a6e3a1" // list bullets
	quoteLavender = "#9399b2" // the quote bar and the quote itself
	linkBlue      = "#89b4fa"
	ruleGrey      = "#585b70"
)

// A heading carries its level on two channels at once, because neither does the whole job.
//
// Hue identifies the level: six distinct colours, one each. That is what colour is good at — it is
// a nominal channel, a set of categories with no inherent order — and it is why hue alone was not
// enough. Level is ordinal, h2 outranks h3, and nothing about peach says it sits under green, so
// six hue-to-level pairings had to be memorised to read the shape of a reply.
//
// Weight supplies that missing order, on channels ordered by nature: a rule, then an underline,
// then bold, then italic. More ink always means a higher level, so rank is readable off prominence
// alone, with no legend to recall. Weight is coarse — it sorts the levels into tiers rather than
// separating all six — and hue tells apart the levels that share a tier. h3 and h4 are both bold
// and differ by hue; h5 and h6 are both italic and differ by hue.
type headingSpec struct {
	colour string
	// rule draws under the heading, sized to the pane. Only h1 has one: h1 is rare in a reply and
	// reads as its title, where a full-width line under every h2 was visibly heavy.
	rule                           string
	upper, underline, bold, italic bool
}

// Ordered h1 to h6. Nothing is prefixed: a bar down the left of each heading was tried and cut —
// it read as hierarchy well enough, but it was redundant beside the underline on h2 and it pushed
// every heading off the flush-left edge the rest of the transcript keeps.
//
// There is no faint step either, though the ramp wants one and StylePrimitive has the field:
// glamour's renderText honours Underline, Bold, Italic, CrossedOut, Inverse, Blink and Upper, and
// simply never reads Faint. Italic plus the grey is what stands in for it at h6.
var headingSpecs = []headingSpec{
	{colour: headingBlue, rule: "═", upper: true, bold: true},
	{colour: headingGreen, underline: true, bold: true},
	{colour: headingPeach, bold: true},
	{colour: headingMauve, bold: true},
	{colour: headingTeal, italic: true},
	{colour: headingGrey, italic: true},
}

// glamour's stock styles are built for rendering whole documents, where a two-column margin and
// a blank line above and below the body are right. In a transcript they are not: the margin
// pushes every reply away from the prompt marker it is supposed to sit beside, and the leading
// blank line drops the first line of text below that marker entirely.
//
// A fenced snippet is also the one place glamour picks code colours, and its stock palette is not
// the one the tools use. Clearing Chroma and naming a theme instead hands the choice to chroma's
// own registry, which is where internal/code gets the same palette — so a fence, a diff and a file
// view agree about what a keyword looks like. Setting Chroma at all would override the theme.
//
// The rest of this is the stock document look traded for a reader's look, after comparing glow
// against leaf on the same source: a heading announces itself with a hue instead of carrying the
// `#` the author typed, a bullet and an ordinal are drawn in an accent while their text stays
// body-coloured, a quote hangs off a bar, and a rule spans the pane. The style has to be built per
// width for that last one — a rule is the one element whose text depends on how wide the pane is —
// which is why this takes a width and why rendererFor caches a renderer per width.
func chatStyle(width int) glamourAnsi.StyleConfig {
	style := styles.DarkStyleConfig
	style.Document.Margin = nil
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Chroma = nil
	style.CodeBlock.Theme = code.Theme

	// The stock H1 is a coloured bar — text on a background, padded with a space either side — and
	// H2 to H6 keep the literal hashes. Both go: the hashes are markup the reader already saw in
	// what they typed, and the bar reads as a highlight rather than as a heading. What replaces
	// them is the weight ramp in headingSpecs.
	//
	// A rule rides in Suffix, which glamour draws with the heading's own style after the text. That
	// is the whole reason this needs no post-processing pass: Format would have been the obvious
	// place for a rule, and it is silently dead here — glamour's cascade copies every other style
	// field down to the text token inside a heading but not Format, so a heading's Format never
	// reaches anything.
	levels := []*glamourAnsi.StyleBlock{
		&style.H1, &style.H2, &style.H3, &style.H4, &style.H5, &style.H6,
	}
	for i, spec := range headingSpecs {
		h := levels[i]
		h.Prefix = ""
		h.Suffix = ""
		h.BackgroundColor = nil
		h.Color = ptr(spec.colour)
		h.Bold = ptr(spec.bold)
		h.Upper = ptr(spec.upper)
		h.Underline = ptr(spec.underline)
		h.Italic = ptr(spec.italic)
		if spec.rule != "" {
			h.Suffix = "\n" + strings.Repeat(spec.rule, width)
		}
	}

	// The bullet goes in Prefix, not BlockPrefix where the stock style keeps it: glamour draws a
	// BlockPrefix with the *parent* block's style — it calls it the unstyled prefix — so a colour
	// set here would be dropped on the floor. Prefix is drawn with the element's own style, and for
	// a bullet it lands in the same column. The item's text is a child element either way, so it
	// keeps the document's colour and only the marker is accented.
	//
	// An ordinal cannot have this. glamour passes the number as the element's own prefix and ". "
	// as the block prefix, and both of those are drawn with the parent's style, so there is no slot
	// left that an enumeration colour would reach. Colouring ordinals means post-processing the
	// rendered output, which is out of scope here.
	style.Item.BlockPrefix = ""
	style.Item.Prefix = "• "
	style.Item.Color = ptr(markerGreen)

	style.BlockQuote.IndentToken = ptr("▏ ")
	style.BlockQuote.Color = ptr(quoteLavender)
	style.BlockQuote.Italic = ptr(true)

	style.Link.Color = ptr(linkBlue)
	style.LinkText.Color = ptr(linkBlue)
	style.LinkText.Bold = ptr(false)

	style.HorizontalRule.Color = ptr(ruleGrey)
	style.HorizontalRule.Format = "\n" + strings.Repeat("─", width) + "\n"

	return style
}

// glamour's style fields are pointers, so that an unset field can cascade from the parent block
// rather than reading as a zero value that overrides it.
func ptr[T any](v T) *T { return &v }

// trimBlock removes the padding glamour adds to fill every line out to the wrap width. A
// transcript line has to end where its text ends: the padding is what gets copied when the user
// selects a reply, and it makes every line measure the full pane width whatever it says.
//
// The padding is styled spaces, so the trailing bytes of a padded line are an escape sequence
// rather than whitespace and strings.TrimRight does nothing. Measuring the visible text and
// truncating to it is what actually cuts it.
//
// Both measurements have to be in columns. Measuring the text in bytes instead compares a byte
// count against a column count, and any line that opens with a multi-byte glyph — a list bullet, a
// quote bar — then measures longer than it draws and keeps its padding.
//
// The blank lines at either end go too, and they are dropped by width rather than by trimming
// newlines: glamour ends a block by styling an empty string, so the first and last lines of a
// reply are usually an escape sequence with no text in it, which is not a newline to trim but
// still costs a row. Blank lines *inside* the block stay — they are the spacing between a heading
// and what it introduces.
func trimBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		visible := ansi.StringWidth(line)
		trimmed := ansi.StringWidth(strings.TrimRight(ansi.Strip(line), " \t"))
		if trimmed < visible {
			lines[i] = ansi.Truncate(line, trimmed, "")
		}
	}
	for len(lines) > 0 && ansi.StringWidth(lines[0]) == 0 {
		lines = lines[1:]
	}
	for len(lines) > 0 && ansi.StringWidth(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// Render lays markdown out for a pane of the given width. It never fails loudly: unrenderable
// input falls back to the source text, because losing a reply to a markdown edge case is worse
// than showing it unstyled.
func Render(md string, width int) string {
	if width < MinWidth {
		width = defaultWidth
	}
	r := rendererFor(width)
	if r == nil {
		return strings.TrimRight(md, "\n")
	}
	out, err := r.Render(markRowRules(drawDiagrams(md, width)))
	if err != nil {
		return strings.TrimRight(md, "\n")
	}
	return trimBlock(drawRowRules(out))
}
