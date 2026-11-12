package markdown

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"tuios.dev/sketch"
	"tuios.dev/sketch/text"
)

// glamour draws a fenced block two columns in, so a diagram has that much less room than prose.
const fenceIndent = 2

// drawDiagrams swaps each closed mermaid fence for its drawing, keeping the source when it cannot be drawn within width.
func drawDiagrams(md string, width int) string {
	if !strings.Contains(md, "mermaid") {
		return md
	}
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		fence, info, ok := openFence(lines[i])
		if !ok {
			out = append(out, lines[i])
			continue
		}
		end := closeFence(lines, i+1, fence)
		if end < 0 {
			// A fence still streaming: everything after it is inside it.
			return strings.Join(append(out, lines[i:]...), "\n")
		}
		if !strings.EqualFold(info, "mermaid") {
			out = append(out, lines[i:end+1]...)
			i = end
			continue
		}
		if drawing, ok := drawMermaid(strings.Join(lines[i+1:end], "\n"), width-fenceIndent); ok {
			out = append(out, "```text", drawing, "```")
		} else {
			out = append(out, lines[i:end+1]...)
		}
		i = end
	}
	return strings.Join(out, "\n")
}

func openFence(line string) (fence, info string, ok bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return "", "", false
	}
	n := len(trimmed) - len(strings.TrimLeft(trimmed, trimmed[:1]))
	if n < 3 {
		return "", "", false
	}
	info = strings.TrimSpace(trimmed[n:])
	if trimmed[0] == '`' && strings.Contains(info, "`") {
		return "", "", false
	}
	if fields := strings.Fields(info); len(fields) > 0 {
		info = fields[0]
	}
	return trimmed[:n], info, true
}

func closeFence(lines []string, start int, fence string) int {
	for i := start; i < len(lines); i++ {
		trimmed := strings.TrimLeft(lines[i], " ")
		if len(lines[i])-len(trimmed) > 3 || !strings.HasPrefix(trimmed, fence) {
			continue
		}
		if strings.TrimSpace(strings.TrimLeft(trimmed, fence[:1])) == "" {
			return i
		}
	}
	return -1
}

func drawMermaid(src string, width int) (string, bool) {
	return cached("mermaid", src, width, func() (drawing string, ok bool) {
		// A panic in the renderer would take the pane down.
		defer func() {
			if recover() != nil {
				drawing, ok = "", false
			}
		}()
		d, err := sketch.Parse(src, sketch.ParseOptions{})
		if err != nil {
			return "", false
		}
		lines, err := text.Draw(d, text.Options{Cols: max(width, 1)})
		if err != nil {
			return "", false
		}
		rows := make([]string, 0, len(lines))
		for _, row := range lines {
			row = strings.TrimRight(row, " ")
			if ansi.StringWidth(row) > width {
				return "", false
			}
			rows = append(rows, row)
		}
		drawing = strings.Trim(strings.Join(rows, "\n"), "\n")
		if strings.TrimSpace(drawing) == "" {
			return "", false
		}
		return drawing, true
	})
}
