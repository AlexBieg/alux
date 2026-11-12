package ui

import (
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
)

// Detail item ids are negative and below noOwner, so they never meet a transcript item's id.
const (
	diffIDBase       = -2
	transcriptIDBase = -1002
)

// transcriptItems are a run's events as transcript items, numbered from base, with each box's default openness.
func transcriptItems(events []subagent.TranscriptEvent, base int) (items []session.Item, open map[int]bool) {
	open = map[int]bool{}
	for i, e := range events {
		id := base - i
		switch e.Kind {
		case subagent.TranscriptTask:
			body := e.Text
			if e.Output != "" {
				body += "\n\n" + e.Output
			}
			items = append(items, session.Item{ID: id, Kind: session.KindTool, Text: "task", Body: strings.TrimSpace(body)})
			open[id] = false
		case subagent.TranscriptText, subagent.TranscriptReport:
			items = append(items, session.Item{ID: id, Kind: session.KindAssistant, Text: e.Text})
		case subagent.TranscriptTool:
			label, kind := e.Label, e.Tool
			if label == "" || label == e.Name {
				var described agent.ToolKind
				label, described = agent.Describe(agent.Call{Name: e.Name, Input: e.Input})
				if kind == "" {
					kind = described
				}
			}
			if e.Failed {
				label += " (failed)"
			}
			item := session.Item{ID: id, Kind: session.KindTool, Tool: kind, Text: label, Body: strings.TrimRight(e.Output, "\n")}
			items = append(items, item)
			open[id] = session.Box{Head: item.Text, Body: item.Body}.OpenByDefault()
		case subagent.TranscriptError:
			items = append(items, session.Item{ID: id, Kind: session.KindError, Text: e.Text})
		}
	}
	return items, open
}

// diffItems split a git diff into one open edit box per file, so each is highlighted for its own language.
func diffItems(text string) []session.Item {
	var items []session.Item
	for i, part := range splitGitDiff(text) {
		items = append(items, session.Item{ID: diffIDBase - i, Kind: session.KindTool, Tool: agent.KindEdit,
			Text: "edit " + part.path, Body: part.body})
	}
	return items
}

type filePatch struct{ path, body string }

// splitGitDiff cuts a git diff at each file header, keeping the hunks and dropping the header lines diff.Render does not draw.
func splitGitDiff(text string) []filePatch {
	var out []filePatch
	var cur *filePatch
	var hunks []string
	flush := func() {
		if cur != nil {
			cur.body = strings.Join(hunks, "\n")
			if cur.body == "" {
				cur.body = "(no textual change)"
			}
			out = append(out, *cur)
		}
		hunks = nil
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if rest, ok := strings.CutPrefix(line, "diff --git "); ok {
			flush()
			path := rest
			if i := strings.Index(rest, " b/"); i >= 0 {
				path = rest[i+3:]
			}
			cur = &filePatch{path: path}
			continue
		}
		if cur == nil {
			continue
		}
		if len(hunks) == 0 && !strings.HasPrefix(line, "@@") {
			continue
		}
		hunks = append(hunks, line)
	}
	flush()
	return out
}
