package ui

import (
	"bytes"
	"regexp"
	"strings"
	"text/tabwriter"

	"charm.land/lipgloss/v2"
)

var (
	pollHeadRe  = regexp.MustCompile(`^poll every (\S+) (until /.*/|while /.*/|until exit 0), gives up after (\S+): (.*)$`)
	pollStampRe = regexp.MustCompile(`^(\d\d:\d\d:\d\d) (.*)$`)
	pollEndRe   = regexp.MustCompile(`^\[(.*)\]$`)
)

// pollHead is a poll box's head taken apart, so the command can lead and the settings follow.
type pollHead struct{ every, cond, limit, command, wake string }

func parsePollHead(s string) (pollHead, bool) {
	var h pollHead
	if i := strings.LastIndex(s, " · check-in "); i >= 0 {
		s, h.wake = s[:i], s[i+len(" · "):]
	}
	m := pollHeadRe.FindStringSubmatch(s)
	if m == nil {
		return h, false
	}
	h.every, h.cond, h.limit, h.command = m[1], m[2], m[3], m[4]
	return h, true
}

type pollEntry struct {
	stamp string
	lines []string
}

// parsePollLog splits a poll's log into one entry per timestamp, and the closing [status] if there is one.
// A body that is still the tool result keeps only its first check.
func parsePollLog(body string) (entries []pollEntry, end string) {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "Polling as proc ") {
		_, body, _ = strings.Cut(body, "First check:\n")
		body, _, _ = strings.Cut(body, "\nNot met yet: ")
	}
	for _, line := range strings.Split(body, "\n") {
		switch m := pollStampRe.FindStringSubmatch(line); {
		case m != nil:
			entries = append(entries, pollEntry{stamp: m[1], lines: []string{m[2]}})
		case pollEndRe.MatchString(line):
			end = pollEndRe.FindStringSubmatch(line)[1]
		case line == "":
		case len(entries) == 0:
			entries = append(entries, pollEntry{lines: []string{line}})
		default:
			e := &entries[len(entries)-1]
			e.lines = append(e.lines, line)
		}
	}
	return entries, end
}

// aligned lays tab-separated rows out in columns, the way gh pr checks means them to be read.
func aligned(lines []string) []string {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, l := range lines {
		_, _ = w.Write([]byte(l + "\n"))
	}
	_ = w.Flush()
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

func pollVerdict(running, live bool, end string) (segment, bool) {
	switch {
	case running:
		return segment{"first check…", dimStyle}, true
	case live:
		return segment{"waiting", lipgloss.NewStyle().Foreground(pendingColor)}, true
	case strings.HasPrefix(end, "condition met"):
		return segment{"✓ " + end, lipgloss.NewStyle().Foreground(lipgloss.Green)}, true
	case strings.HasPrefix(end, "gave up"), strings.HasPrefix(end, "stopped:"):
		return segment{"✗ " + end, errorStyle}, true
	}
	return segment{end, dimStyle}, end != ""
}

type segment struct {
	text string
	st   lipgloss.Style
}

// packSegments joins segments with " · " into rows of width, breaking between them where it can.
func packSegments(segs []segment, width int) []string {
	sep := dimStyle.Render(" · ")
	var rows []string
	row, used := "", 0
	for _, s := range segs {
		w := lipgloss.Width(s.text)
		switch {
		case row != "" && used+3+w <= width:
			row, used = row+sep+s.st.Render(s.text), used+3+w
			continue
		case row != "":
			rows = append(rows, row)
		}
		lines := styledWrap(s.text, s.st, width)
		rows = append(rows, lines[:len(lines)-1]...)
		row, used = lines[len(lines)-1], lipgloss.Width(lines[len(lines)-1])
	}
	if row != "" {
		rows = append(rows, row)
	}
	return rows
}

// renderPollBox draws the command first, the settings and verdict on one dim row under it, then the log.
func renderPollBox(h pollHead, body, lead string, tail []string, leadWidth int, paint func(string) string, running, live, open bool, width int) string {
	out := wrapPlain(h.command, max(1, width-leadWidth))
	pad := strings.Repeat(" ", leadWidth)
	for i, line := range out {
		if i == 0 {
			out[i] = lead + paint(line)
			continue
		}
		out[i] = pad + paint(line)
	}

	entries, end := parsePollLog(body)
	var meta []segment
	for _, s := range []string{h.cond, "every " + h.every, "up to " + h.limit, h.wake} {
		if s != "" {
			meta = append(meta, segment{s, dimStyle})
		}
	}
	if v, ok := pollVerdict(running, live, end); ok {
		meta = append(meta, v)
	}
	for _, styled := range tail {
		meta = append(meta, segment{text: styled})
	}
	for _, row := range packSegments(meta, max(1, width-leadWidth)) {
		out = append(out, pad+row)
	}
	if !open || running {
		return strings.Join(out, "\n")
	}

	gutter := len("00:00:00 ")
	for _, e := range entries {
		for i, line := range aligned(e.lines) {
			prefix := pad + strings.Repeat(" ", gutter)
			if i == 0 && e.stamp != "" {
				prefix = pad + dimStyle.Render(e.stamp) + " "
			}
			out = append(out, hang(prefix, leadWidth+gutter, line, dimStyle, width)...)
		}
	}
	return strings.Join(out, "\n")
}
