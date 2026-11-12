package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/records"
)

// Markdown for the pane alone: the model is answered with the compact text, so nothing here costs context.

// keepLines makes each line of plain text its own paragraph: glamour joins single newlines and honours no hard break.
func keepLines(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n\n")
}

func writtenMarkdown(in records.RecordInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n%s\n", in.Title, metaLine(in.Kind, in.Subject, "", in.Confidence, in.Sensitivity, 0, ""))
	if in.Text != "" {
		fmt.Fprintf(&b, "\n%s\n", keepLines(in.Text))
	}
	var items []string
	items = append(items, attrItems(in.Attrs)...)
	if in.ValidFrom != "" || in.ValidUntil != "" {
		items = append(items, fmt.Sprintf("- **valid**: %s → %s", or(in.ValidFrom, "?"), or(in.ValidUntil, "now")))
	}
	if len(in.Supersedes) > 0 {
		items = append(items, "- **supersedes**: "+codeList(in.Supersedes))
	}
	if len(in.Links) > 0 {
		items = append(items, "- **links**: "+linkList(in.Links))
	}
	if len(in.DropLinks) > 0 {
		items = append(items, "- **drops links**: "+linkList(in.DropLinks))
	}
	if src := sourceLine(in.Source); src != "" {
		items = append(items, "- **source**: "+src)
	}
	if len(items) > 0 {
		fmt.Fprintf(&b, "\n%s\n", strings.Join(items, "\n"))
	}
	if in.Body != "" {
		fmt.Fprintf(&b, "\n---\n\n%s\n", strings.TrimRight(in.Body, "\n"))
	}
	return strings.TrimRight(b.String(), "\n")
}

func searchMarkdown(rows []records.Row, note string) string {
	var b strings.Builder
	rest := func(r records.Row) bool { return r.Partial || r.Linked || r.Similar }
	for i, r := range rows {
		if rest(r) {
			if i == 0 || !rest(rows[i-1]) {
				if i == 0 {
					b.WriteString("*No record matches every word.*\n\n")
				}
				b.WriteString("#### Partial matches, close in meaning, or linked\n\n")
			}
			tag := ""
			switch {
			case r.Linked:
				tag = " · linked"
			case r.Similar:
				tag = " · similar"
			}
			fmt.Fprintf(&b, "- %s — %s%s\n", r.Title, rowMeta(r), tag)
			if i+1 == len(rows) || !rest(rows[i+1]) {
				b.WriteString("\n")
			}
			continue
		}
		fmt.Fprintf(&b, "### %s\n%s\n", r.Title, rowMeta(r))
		if r.Text != "" {
			fmt.Fprintf(&b, "\n%s\n", keepLines(r.Text))
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(r.Attrs), &m)
		items := attrItems(m)
		if r.ValidFrom != "" || r.ValidUntil != "" {
			items = append(items, fmt.Sprintf("- **valid**: %s → %s", or(r.ValidFrom, "?"), or(r.ValidUntil, "now")))
		}
		if len(items) > 0 {
			fmt.Fprintf(&b, "\n%s\n", strings.Join(items, "\n"))
		}
		b.WriteString("\n")
	}
	if note != "" {
		fmt.Fprintf(&b, "*%s*\n", keepLines(note))
	}
	return strings.TrimRight(b.String(), "\n")
}

func rowMeta(r records.Row) string {
	status := ""
	if r.Status != "active" {
		status = r.Status
	}
	return metaLine(r.Kind, r.Subject, r.ID, r.Confidence, r.Sensitivity, r.Sources, status)
}

func metaLine(kind, subject, id, confidence, sensitivity string, sources int, status string) string {
	parts := []string{fmt.Sprintf("**%s** about **%s**", kind, subject)}
	if id != "" {
		parts = append([]string{"`" + id + "`"}, parts...)
	}
	if status != "" {
		parts = append(parts, status)
	}
	if confidence != "" && confidence != "high" {
		parts = append(parts, confidence+" confidence")
	}
	if sources > 1 {
		parts = append(parts, fmt.Sprintf("%d sources", sources))
	}
	if sensitivity == "phi" {
		parts = append(parts, "phi")
	}
	return strings.Join(parts, " · ")
}

func attrItems(attrs map[string]any) []string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []string
	for _, k := range keys {
		switch v := attrs[k].(type) {
		case []any:
			out = append(out, fmt.Sprintf("- **%s**:", k))
			for _, e := range v {
				out = append(out, "  - "+attrValue(e))
			}
		case map[string]any:
			out = append(out, fmt.Sprintf("- **%s**:", k))
			for _, item := range attrItems(v) {
				out = append(out, "  "+item)
			}
		default:
			out = append(out, fmt.Sprintf("- **%s**: %s", k, attrValue(v)))
		}
	}
	return out
}

func attrValue(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+attrValue(v[k]))
		}
		return strings.Join(parts, " · ")
	}
	j, _ := json.Marshal(v)
	return string(j)
}

func codeList(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = "`" + id + "`"
	}
	return strings.Join(out, ", ")
}

func linkList(links []records.Link) string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		target := l.Entity
		if l.Record != "" {
			target = "`" + l.Record + "`"
		}
		out = append(out, l.Role+" "+target)
	}
	return strings.Join(out, " · ")
}

func sourceLine(s records.Source) string {
	parts := []string{}
	for _, p := range []string{s.Type, s.Ref, s.Title, s.OccurredAt} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " · ")
}
