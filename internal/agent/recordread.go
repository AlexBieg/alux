package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/records"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/anthropics/anthropic-sdk-go"
)

var recordReadDescription = `Read the authoritative record store. Current records only unless include_history is set.

op "search": ranked full-text search over record titles, text, bodies, attrs and subject names and aliases, optionally narrowed by kind and subject. Common words like "why" and "the" are ignored. Records matching every word come first; any after them match only some words, are close in meaning, or are linked to the matches, and are marked so (none of these with include_history, as_of or known_at), and a reply with no full match says so. A full match shows its title, text and validity; the rest show the title line only; attrs are left to get. With no query, lists the newest matching records.
op "get": one record in full: attrs, source, who recorded it, status events, links both ways, and its supersession chain.
get and about add an "episode:" line to a record whose session has a gist: its title and the session id. Read it with op "episode" only when the record does not answer why or how it came about.
op "episode": a session's gist (goal, outcome, turn points, surprises, what was tried and dropped, artifacts) and the current records it wrote (session = the id from an episode line).
op "about": an entity's description, aliases and merged names, and every current record about it grouped by kind, then the records filed under another subject that link to it with role about. Pass kind to read one kind. An entity with more than ` + strconv.Itoa(aboutFullMax) + ` current records and mentions together, and no kind given, is listed as titles only; read the rest with kind or get.
op "ungisted": sessions that wrote records and have no gist yet, whose transcript is still on disk and whose alux has exited, oldest first, so the first is the next to be pruned; each with its transcript path. The session reading it is never listed. Gist one with vault_record op "episode".
op "conflicts": every subject with two or more current states where a state has no slot or two share one, the list a slot cleanup works down. Subjects holding phi states are counted, not named; the cleanup is done when nothing is listed and that count is zero.
op "unfiled": open entries agents kept for review because they could not be stored. Close one once it is dealt with using vault_record op "resolve".
op "pairs": pairs of current records whose embeddings sit at cosine ` + fmt.Sprintf("%.2f", records.PairFloor) + ` or above, with no link between them and no verdict yet, closest first (limit, default 10; above 50 falls back to 10). Records marked phi are never paired. Read both with op get, then link, merge or leave them, and close each with vault_record op "judge".

search, get and about read the present by default. as_of reads what held in the world on a date (records outside their valid_from..valid_until show as not_yet or expired); known_at reads what the store had recorded by then, before later supersessions, retractions, drops and records. Either alone leaves the other at now.`

// Past this many current records an unfiltered about lists titles, so a large project stays cheap to read.
const aboutFullMax = 25

// SessionDir holds main-session transcripts, one <session>.jsonl each.
func SessionDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, paths.State, "sessions")
}

type GistableSession struct {
	Session    string
	Transcript string
	Ended      time.Time
}

// GistableSessions is every ungisted session other than self and those running whose transcript in dir still exists, oldest first.
func GistableSessions(store *records.Store, dir, self string, running map[string]bool) ([]GistableSession, error) {
	ids, err := store.UngistedSessions()
	if err != nil || dir == "" {
		return nil, err
	}
	var out []GistableSession
	for _, id := range ids {
		if id == self || running[id] {
			continue
		}
		path := filepath.Join(dir, filepath.Base(id)+".jsonl")
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		out = append(out, GistableSession{Session: id, Transcript: path, Ended: info.ModTime()})
	}
	slices.SortFunc(out, func(a, b GistableSession) int { return a.Ended.Compare(b.Ended) })
	return out, nil
}

// This replaces the vault's "tell the user in one line" rule: capture goes to records.
func RecordSystemNote() string {
	if records.Path() == "" {
		return ""
	}
	return "The user keeps a second, authoritative knowledge store: typed records about named entities, written with `vault_record` and read with `vault_record_read`. Each record is one claim, state, decision, measurement, todo, question, procedure, rule or argument about an entity, with its source. Records are never edited: a change is a new record that supersedes the old one." +
		" The markdown vault is read-only, but this store is not: when you learn something durable, store it with `vault_record` as you go, without being asked, rather than only mentioning it to the user. Check `vault_record_read` first so you supersede an existing record instead of duplicating it. If a write is refused, read the reason and fix the request; if it truly fits nowhere, keep it with op unfile. Tell the user in one line what you stored."
}

func RecordReadTools() []anthropic.ToolUnionParam {
	if records.Path() == "" {
		return nil
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name:        "vault_record_read",
		Description: anthropic.String(recordReadDescription),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"op":              map[string]any{"type": "string", "enum": []string{"search", "get", "about", "episode", "ungisted", "conflicts", "unfiled", "pairs"}},
				"session":         str("episode: the session id, from an episode line."),
				"query":           str("search: what you want to know, in words."),
				"kind":            map[string]any{"type": "string", "enum": []string{"claim", "state", "decision", "measurement", "todo", "question", "procedure", "rule", "argument"}, "description": "search, about: only this kind."},
				"subject":         str("search: only records about this entity; about: the entity, by name or alias."),
				"include_history": map[string]any{"type": "boolean", "description": "search: include superseded, retracted, dropped, done, expired and not-yet-valid records."},
				"limit":           map[string]any{"type": "integer", "description": "search, unfiled, pairs: at most this many, default 10, up to 50; a larger value falls back to 10."},
				"id":              str("get: the record id."),
				"as_of":           str("search, get, about: YYYY-MM-DD, read what held in the world on that day."),
				"known_at":        str("search, get, about: YYYY-MM-DD or an RFC 3339 time, read what the store had recorded by then."),
			},
			Required:    []string{"op"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}}
}

func RecordReadHandlers() map[string]Handler {
	if records.Path() == "" {
		return nil
	}
	return map[string]Handler{"vault_record_read": runRecordRead}
}

func runRecordRead(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Op string `json:"op"`
		records.SearchInput
		ID      string `json:"id"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	label := "records: " + in.Op
	asOf, knownAt := in.ValidAt, in.KnownAt
	if asOf != "" {
		asOf = "as of " + asOf
	}
	if knownAt != "" {
		knownAt = "known at " + knownAt
	}
	for _, p := range []string{in.Query, in.Kind, in.Subject, in.ID, in.Session, asOf, knownAt} {
		if p != "" {
			label += " · " + p
		}
	}
	cb.toolStart(call.ID, label, KindVault)

	id := in.ID
	switch in.Op {
	case "episode":
		id = in.Session
	case "ungisted":
		id = recordSessionOf(ctx)
	}
	output, shown, err := readRecords(in.Op, in.SearchInput, id)
	if err != nil {
		cb.toolResult(call.ID, label, err.Error(), KindVault)
		return failed(call.ID, err)
	}
	if shown == "" {
		cb.toolResult(call.ID, label, output, KindVault)
	} else {
		cb.toolResult(call.ID, label, shown, KindRecord)
	}
	return ok(call.ID, output)
}

// readRecords answers the model with output and, for ops that have one, the pane with markdown in shown.
func readRecords(op string, search records.SearchInput, id string) (output, shown string, err error) {
	output, err = readRecordsText(op, search, id, &shown)
	return output, shown, err
}

func readRecordsText(op string, search records.SearchInput, id string, shown *string) (string, error) {
	store, err := openRecordStore()
	if err != nil {
		return "", fmt.Errorf("record store unavailable: %w", err)
	}
	var b strings.Builder
	switch op {
	case "search":
		rows, note, err := store.SearchNote(search)
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			if note != "" {
				return "No matching records. Try other words, or include_history.\n" + note, nil
			}
			return "No matching records. Try other words, or include_history.", nil
		}
		*shown = searchMarkdown(rows, note)
		rest := func(r records.Row) bool { return r.Partial || r.Linked || r.Similar }
		for i, r := range rows {
			if rest(r) && (i == 0 || !rest(rows[i-1])) {
				if i == 0 {
					b.WriteString("No record matches every word. Partial matches, records close in meaning, and records linked to them:\n")
				} else {
					b.WriteString("\nMatching only some words, close in meaning, or linked to the matches:\n")
				}
			}
			switch {
			case r.Linked:
				b.WriteString("(linked) ")
			case r.Similar:
				b.WriteString("(similar) ")
			}
			writeHit(&b, r)
		}
		if note != "" {
			b.WriteString("\n" + note + "\n")
		}
	case "get":
		d, err := store.GetAt(id, search.AsOf)
		if err != nil {
			return "", err
		}
		writeRow(&b, d.Row, true)
		fmt.Fprintf(&b, "  source: %s\n  recorded: %s by %s\n", d.Source, d.RecordedAt, d.RecordedBy)
		writeEpisode(&b, d.Row)
		list := func(name string, items []string) {
			for _, it := range items {
				fmt.Fprintf(&b, "  %s: %s\n", name, it)
			}
		}
		list("also cited", d.Cited)
		list("event", d.Events)
		list("link", d.Links)
		list("linked from", d.Backlinks)
		list("replaces", d.Predecessors)
		if d.Successor != "" {
			fmt.Fprintf(&b, "  replaced by: %s\n", d.Successor)
		}
	case "episode":
		ep, err := store.GetEpisode(id)
		if err != nil {
			return "", err
		}
		phi := ""
		if ep.Sensitivity == "phi" {
			phi = ", phi"
		}
		fmt.Fprintf(&b, "%s (session %s%s, gisted %s)\n  goal: %s\n  outcome: %s\n", ep.Title, ep.Session, phi, ep.RecordedAt[:10], ep.Gist.Goal, ep.Gist.Outcome)
		for _, part := range []struct {
			name  string
			items []string
		}{{"turn point", ep.Gist.TurnPoints}, {"surprise", ep.Gist.Surprises}, {"tried and dropped", ep.Gist.TriedAndDropped}, {"artifact", ep.Gist.Artifacts}} {
			for _, it := range part.items {
				fmt.Fprintf(&b, "  %s: %s\n", part.name, it)
			}
		}
		if len(ep.Records) > 0 {
			b.WriteString("\nCurrent records it wrote:\n")
			for _, r := range ep.Records {
				writeTitle(&b, r)
			}
		}
	case "ungisted":
		sessions, err := GistableSessions(store, SessionDir(), id, registry.RunningSessions())
		if err != nil {
			return "", err
		}
		if len(sessions) == 0 {
			return "No sessions to gist: every session that wrote records has a gist, is still running, or has had its transcript pruned.", nil
		}
		fmt.Fprintf(&b, "%d ungisted session(s), oldest first:\n", len(sessions))
		for _, s := range sessions {
			fmt.Fprintf(&b, "%s (ended %s) transcript: %s\n", s.Session, s.Ended.Format(time.RFC3339), s.Transcript)
		}
	case "about":
		e, err := store.AboutAt(search.Subject, search.Kind, search.AsOf)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s (%s, %s)\n", e.Name, e.Type, e.ID)
		if e.Description != "" {
			fmt.Fprintf(&b, "%s\n", e.Description)
		}
		if len(e.Aliases) > 0 {
			fmt.Fprintf(&b, "aliases: %s\n", strings.Join(e.Aliases, ", "))
		}
		if len(e.Merged) > 0 {
			fmt.Fprintf(&b, "merged in: %s\n", strings.Join(e.Merged, ", "))
		}
		if len(e.Records) == 0 && len(e.Mentions) == 0 {
			if search.Kind != "" {
				fmt.Fprintf(&b, "No current %s records.\n", search.Kind)
			} else {
				b.WriteString("No current records.\n")
			}
		}
		brief := search.Kind == "" && len(e.Records)+len(e.Mentions) > aboutFullMax
		if brief {
			fmt.Fprintf(&b, "%d current records and %d mentions, so titles only: pass kind to read one kind in full, or get one id.\n",
				len(e.Records), len(e.Mentions))
		}
		write := func(r records.Row) {
			if brief {
				writeTitle(&b, r)
			} else {
				writeRow(&b, r, false)
				writeEpisode(&b, r)
			}
		}
		kind := ""
		for i, r := range e.Records {
			if r.Kind != kind {
				kind = r.Kind
				n := 0
				for _, o := range e.Records[i:] {
					if o.Kind == kind {
						n++
					}
				}
				fmt.Fprintf(&b, "\n## %s (%d)\n", kind, n)
			}
			write(r)
		}
		if len(e.Mentions) > 0 {
			fmt.Fprintf(&b, "\n## mentioned in (%d)\n", len(e.Mentions))
			for _, r := range e.Mentions {
				write(r)
			}
		}
	case "conflicts":
		cs, phi, err := store.Conflicts()
		if err != nil {
			return "", err
		}
		if len(cs) == 0 && phi == 0 {
			return "No overlapping current states: every subject with two or more has each in its own slot.", nil
		}
		if len(cs) > 0 {
			fmt.Fprintf(&b, "%d subject(s) with overlapping current states.\n", len(cs))
		}
		for _, c := range cs {
			fmt.Fprintf(&b, "\n## %s (%d states)\n", c.Subject, len(c.States))
			if len(c.Shared) > 0 {
				fmt.Fprintf(&b, "shared slot: %s\n", strings.Join(c.Shared, ", "))
			}
			for _, r := range c.States {
				slot := "no slot"
				if r.Slot != "" {
					slot = "slot " + r.Slot
				}
				fmt.Fprintf(&b, "%s [%s] %s\n", r.ID, slot, r.Title)
			}
		}
		if phi > 0 {
			fmt.Fprintf(&b, "\n%d subject(s) holding phi states also overlap. They are not named here, so cleaning them up is a job for a session that means to read phi.\n", phi)
		}
	case "unfiled":
		items, err := store.OpenUnfiled(search.Limit)
		if err != nil {
			return "", err
		}
		if len(items) == 0 {
			return "No open unfiled entries.", nil
		}
		for _, u := range items {
			fmt.Fprintf(&b, "#%d [%s] %s (%s)\n", u.ID, u.Reason, u.Summary, u.At)
			if u.Error != "" {
				fmt.Fprintf(&b, "  error: %s\n", u.Error)
			}
			fmt.Fprintf(&b, "  payload: %s\n", u.Payload)
		}
	case "pairs":
		limit := search.Limit
		if limit <= 0 || limit > 50 {
			limit = 10
		}
		pairs, total, err := store.Pairs(limit)
		if err != nil {
			return "", err
		}
		if total == 0 {
			return fmt.Sprintf("No unjudged pairs: every pair of current records at cosine %.2f or above is linked or has a verdict.", records.PairFloor), nil
		}
		fmt.Fprintf(&b, "%d unjudged pair(s) at cosine %.2f or above; the %d closest:\n", total, records.PairFloor, len(pairs))
		for _, p := range pairs {
			fmt.Fprintf(&b, "\ncosine %.3f\n", p.Cosine)
			writeTitle(&b, p.A)
			writeTitle(&b, p.B)
		}
	default:
		return "", fmt.Errorf("unknown op %q: use search, get, about, episode, ungisted, conflicts, unfiled or pairs", op)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// The subject sits inside the brackets, so what follows them is exactly the stored title and can be copied as one.
func writeTitle(b *strings.Builder, r records.Row) {
	flags := ""
	if r.Confidence != "high" {
		flags += ", " + r.Confidence + " confidence"
	}
	if r.Sensitivity == "phi" {
		flags += ", phi"
	}
	fmt.Fprintf(b, "%s [%s about %s%s] %s\n", r.ID, r.Kind, r.Subject, flags, r.Title)
}

func writeEpisode(b *strings.Builder, r records.Row) {
	if r.Episode == "" {
		return
	}
	fmt.Fprintf(b, "  episode: %s (session %s)\n", r.Episode, r.Session)
}

// writeHit is a search row: a full match without attrs, anything else as its head line alone.
func writeHit(b *strings.Builder, r records.Row) {
	writeHead(b, r)
	if r.Partial || r.Linked || r.Similar {
		return
	}
	if r.Text != "" {
		fmt.Fprintf(b, "  %s\n", r.Text)
	}
	writeValid(b, r)
}

func writeRow(b *strings.Builder, r records.Row, full bool) {
	writeHead(b, r)
	if r.Text != "" {
		fmt.Fprintf(b, "  %s\n", r.Text)
	}
	if a := records.AttrsLine(r.Attrs); a != "" {
		fmt.Fprintf(b, "  attrs: %s\n", a)
	}
	writeValid(b, r)
	if full && r.Body != "" {
		fmt.Fprintf(b, "\n%s\n\n", r.Body)
	}
}

func writeValid(b *strings.Builder, r records.Row) {
	if r.ValidFrom != "" || r.ValidUntil != "" {
		fmt.Fprintf(b, "  valid: %s → %s\n", or(r.ValidFrom, "?"), or(r.ValidUntil, "now"))
	}
}

func writeHead(b *strings.Builder, r records.Row) {
	fmt.Fprintf(b, "%s [%s about %s", r.ID, r.Kind, r.Subject)
	if r.Status != "active" {
		fmt.Fprintf(b, ", %s", r.Status)
	}
	if r.Confidence != "high" {
		fmt.Fprintf(b, ", %s confidence", r.Confidence)
	}
	if r.Sources > 1 {
		fmt.Fprintf(b, ", %d sources", r.Sources)
	}
	if r.Sensitivity == "phi" {
		b.WriteString(", phi")
	}
	fmt.Fprintf(b, "] %s\n", r.Title)
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
