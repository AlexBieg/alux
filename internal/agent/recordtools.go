package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/llm"
	"github.com/alexbieg/alux/internal/records"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/anthropics/anthropic-sdk-go"
)

var (
	recordStoreMu sync.Mutex
	recordStores  = map[string]*records.Store{}
)

// OpenRecordStore is the record store the record tools use, shared with callers outside them.
func OpenRecordStore() (*records.Store, error) { return openRecordStore() }

// A failed open is retried and reported on every call rather than hiding the tool, so a broken path shows.
func openRecordStore() (*records.Store, error) {
	path := records.Path()
	recordStoreMu.Lock()
	defer recordStoreMu.Unlock()
	if s, ok := recordStores[path]; ok {
		return s, nil
	}
	s, err := records.Open(path)
	if err != nil {
		return nil, err
	}
	if e := records.EmbedderFromEnv(); e != nil {
		if err := s.SetEmbedder(e); err != nil {
			s.Close()
			return nil, err
		}
	}
	recordStores[path] = s
	return s, nil
}

const recordToolDescription = `Store knowledge in the authoritative record store. A refused write stores nothing and the reply says what was wrong: fix the request and try again, or, if it genuinely cannot be stored, keep it for review with op "unfile" (reason "rejected", passing the error).

op "add": one typed record about an existing entity. Kinds and their attrs:
- claim: a fact, trap or behaviour. attrs: consequence, workaround, verified_how
- state: current status. attrs: value*, as_of* (YYYY-MM-DD), slot (a short name for what it describes, like pr-heads or allocation: only one current state per subject may fill a slot, so a new reading must supersede the one holding it)
- decision: what was chosen. attrs: chosen*, rejected (list of {option, reason}), decided_by (entity)
- measurement: attrs: metric*, value* (number), unit, n, method
- todo: attrs: gate, due (date), owner (entity), priority (p1|p2|p3)
- question: attrs: options (list), owner (entity)
- procedure: attrs: when, steps (list); may carry body
- rule: a standing instruction. attrs: scope, how_to_apply, trigger (when code should apply it: a list of clauses, any of which fires, each an object whose keys must all match; keys are repo (a repo entity, by name or root path), tool (tool name), bash (command prefix), path (glob of a file read or edited, relative to the repo root, where ** spans any number of folders), agent (a subagent name, like coder: the rule is in that subagent's starting set, or with other keys fires only inside it), or always: true or topic (a short topic) alone in their clause; e.g. [{"repo": "acme/app", "path": "web/**/*.tsx"}, {"tool": "grep"}]), mode (hold|advise: hold stops the matching call until the rule is read, advise attaches it to the result), when (hold rules only: a CEL condition, so the rule holds a matching call only while it is true and is otherwise shown as advice; variables call.tool, call.args (the tool input), call.commands (list of shell commands), call.path, session.cwd, session.repo (repo entity id) and session.agent ("" for the main agent); functions words(s), cmd(c, prefix), hasFlag(c, flag), positional(c) and broadRoot(path) (true for /, a directory directly under it, home, a non-dot directory directly under home, ~/.local/share or ~/.local/state; a relative path resolves against session.cwd), revs(c) (a jj command's target revisions: -r, -s, -b values and bare arguments, @ when none), descendants(rev) (change ids stacked on rev, from jj) and listed(rev) (an earlier jj log this conversation named rev inside dragged() or descendants()); e.g. call.tool == "pr_create" && has(call.args.body) && words(call.args.body) > 400). Write the title as the instruction itself. Agents are sent only the title and how_to_apply, so how_to_apply must hold every step and constraint needed to follow the rule; text holds only why (the incident, date, who said it), read on demand. Give the narrowest trigger that catches the moment it applies; use always only for a rule nearly every session needs. A topic-only rule needs a topic entity whose description says, in one line, when to read it.
- argument: reasoning that must stay prose. attrs: thesis*; body required
(* required.) Every add also requires sensitivity, which has no default: phi for sensitive personal data (health or patient details, for example), normal otherwise. An entity attr also links the record to that entity with role about, so it is listed under it too. title is the one line, without the subject: reads show the subject separately, so never copy it into a title. text is at most 600 chars of why/workaround. A record's content is never edited: to change its title, text, attrs or confidence, add a new record of the same kind with supersedes set to its id. When two current records say the same thing (an older and a newer reading of one slot, say), write one record with supersedes set to both ids, never a retraction. A replaced record still holds in as_of reads before the new record's valid_from, so set valid_from on the new one when the old reading was true until then. Superseding carries the old records' about, see_also, refines and depends_on links over (pointing at each target's current version); list in drop_links any of those the new content no longer warrants. Their contradicts, supports and answers links and their citations are not carried, nor the own source of any record after the first in supersedes: the reply lists them, and you re-add with op "link" or "cite" the ones that still hold. Never supersede only to add a link or a slot; use op "link" or "slot". To answer a question, add the answer with a link {role: answers, record: <question id>}, then mark the question done.

Search the store before writing, so nothing is duplicated. After an add or entity is written, the reply may list existing records or entities that look similar. Act on one only when it warrants it: link a record that backs or depends on yours with op "link"; when one states the same fact, write one record with supersedes set to both ids; when an entity names the same thing, fold yours into it with entity_edit merge_into. Otherwise ignore the list.

valid_from and valid_until bound when the record held in the world, both days included; reads can ask what held as of a date. valid_until is never "the day it was replaced": replacement is supersession, and a record that stays true as history keeps valid_until unset.

op "link": add links to an existing current record (record = its id, links as for add). Links are append-only and shown on both ends, so one link per pair is enough. Refused if either record was superseded, retracted or dropped; the refusal names the current id. A link that already exists is skipped.
op "cite": add a further source that independently states an existing current record's content (record = its id, source as for add), instead of writing a near-copy. Reads show how many distinct sources back a record; citing the same source again, or the record's own source, adds nothing. Citations are not carried when the record is superseded.
op "slot": file a current state written without a slot into one (record = its id, slot = the name), keeping its id, links and citations. Set once: refused when the state already has a slot, or when another current state on the subject holds that slot (supersede that one instead).
op "event": mark a record done or reopened (todos and questions only), retracted or dropped (both final, reason required). Retract only what was never true. Drop what was true when written but does not belong in the store, such as transient PR or CI state; like a retraction it leaves current and as_of reads and shows only in history. A record that was true, is now outdated and is still worth knowing is superseded.
op "entity": create something records can be about, when no existing name or alias fits (entity: type, name, sensitivity, optional description and aliases).
op "entity_edit": change an existing entity named by entity.name (any name or alias): add entity.aliases, replace entity.description, or set entity.merge_into to fold a duplicate into the entity that should remain. A merge is final and needs a reason; afterwards the duplicate's name and aliases resolve to the remaining entity, and its records and links are listed under it. With merge_into, aliases and description apply to the remaining entity.
op "unfile": keep something that fits nowhere, for later review, instead of forcing it into a record.
op "resolve": close an open unfiled entry (unfiled = its #number from vault_record_read op unfiled) once it is dealt with. outcome "filed" when it became a record (record = that id); "schema_changed" when the store gained a way to do what it asked and that was done; "discarded" when it should not be stored. Without a record, give a reason. A resolution is final.
op "judge": give a verdict on a pair of current records from vault_record_read op pairs (record and other = the two ids, verdict, optional reason): same when they state one fact, related when they belong linked, distinct when they only look alike. Any verdict takes the pair out of the pairs queue; the verdict does not link or merge anything, so do that with op link or a superseding add first. reopened puts a judged pair back. A record that is later superseded comes back as a new pair under its new id.
op "episode": write a finished session's gist once (session, title, gist, sensitivity), from its transcript. Refused for a session that wrote no records or already has a gist. Records that session wrote then show an episode line in reads.`

func RecordTools() []anthropic.ToolUnionParam {
	if records.Path() == "" {
		return nil
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	enum := func(desc string, values ...string) map[string]any {
		return map[string]any{"type": "string", "enum": values, "description": desc}
	}
	sensitivity := enum("add: required, no default. unfile: optional, kept as phi when unset. phi for sensitive personal data (health or patient details, for example); normal otherwise.", "normal", "phi")
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name:        "vault_record",
		Description: anthropic.String(recordToolDescription),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"op":      enum("What to do.", "add", "link", "cite", "slot", "event", "entity", "entity_edit", "unfile", "resolve", "judge", "episode"),
				"session": str("episode: the session to gist, as named by its transcript file."),
				"gist": map[string]any{
					"type":        "object",
					"description": "episode: the session's gist. goal and outcome are required; each list item is one line, keeping identifiers, paths, numbers and error strings verbatim, since the transcript is pruned and the gist is what remains.",
					"properties": map[string]any{
						"goal":              str("What the session set out to do."),
						"outcome":           str("Where it ended: what was built, decided or left."),
						"turn_points":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Moments the direction changed, and who changed it."},
						"surprises":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Results nobody would have predicted from the goal."},
						"tried_and_dropped": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Each approach abandoned, with why."},
						"artifacts":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Files, changes, probes and paths it left."},
					},
					"required": []string{"goal", "outcome"},
				},
				"kind":        enum("add: the record's kind.", "claim", "state", "decision", "measurement", "todo", "question", "procedure", "rule", "argument"),
				"subject":     str("add: the entity this is about, by name or alias."),
				"title":       str("add: the one-line claim, rule, choice or task, not starting with the subject."),
				"text":        str("add: up to 600 chars of why, workaround or steps."),
				"body":        str("add: prose, for argument and procedure only."),
				"attrs":       map[string]any{"type": "object", "description": "add: the kind's fields, listed in the tool description."},
				"valid_from":  str("add: YYYY-MM-DD the record became true, when it matters."),
				"valid_until": str("add: YYYY-MM-DD, the last day it held in the world, when it stopped holding. Never the day it was replaced: supersede for that."),
				"supersedes": map[string]any{
					"description": "add: id of the record of the same kind this replaces, or a list of ids when it replaces several.",
					"anyOf":       []any{map[string]any{"type": "string"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
				},
				"confidence":  enum("add: defaults to high.", "high", "medium", "low"),
				"sensitivity": sensitivity,
				"source": map[string]any{
					"type":        "object",
					"description": "add: where this came from. cite: the further source.",
					"properties": map[string]any{
						"type":        enum("", "conversation", "meeting", "pr", "ticket", "url", "file", "observation"),
						"ref":         str("A stable reference: URL, PR number, ticket key, file path, or a short description of the conversation."),
						"title":       str(""),
						"occurred_at": str("YYYY-MM-DD"),
					},
					"required": []string{"type", "ref"},
				},
				"links": map[string]any{
					"type":        "array",
					"description": "add, link: typed links to records by id or entities by name.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"role":   enum("", "about", "supports", "contradicts", "refines", "depends_on", "answers", "see_also"),
							"record": str("A record id."),
							"entity": str("An entity name or alias."),
						},
						"required": []string{"role"},
					},
				},
				"drop_links": map[string]any{
					"type":        "array",
					"description": "add with supersedes: links on the old record not to carry over, in the shape of links.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"role":   enum("", "about", "see_also", "refines", "depends_on"),
							"record": str("A record id."),
							"entity": str("An entity name or alias."),
						},
						"required": []string{"role"},
					},
				},
				"record":  str("event, link, cite, slot: the record id. resolve: the record the entry became. judge: one record of the pair."),
				"other":   str("judge: the other record of the pair."),
				"verdict": enum("judge: what the pair is.", "same", "related", "distinct", "reopened"),
				"slot":    str("slot: the slot name to file the state under, like pr-heads."),
				"event":   enum("event: what happened.", "done", "reopened", "retracted", "dropped"),
				"reason":  str("event, entity_edit with merge_into, resolve, judge: why, in a few words."),
				"unfiled": map[string]any{"type": "integer", "description": "resolve: the unfiled entry's number."},
				"outcome": enum("resolve: how the entry was closed.", "filed", "schema_changed", "discarded"),
				"entity": map[string]any{
					"type":        "object",
					"description": "entity: the entity to create. entity_edit: the entity to change, by name, and the change.",
					"properties": map[string]any{
						"type":        enum("entity: required.", "person", "system", "ticket", "project", "repo", "term", "team", "org", "topic"),
						"name":        str("entity: canonical name. entity_edit: any name or alias of the entity to change."),
						"description": str("One line, at most 300 chars."),
						"sensitivity": enum("entity: required. phi for sensitive personal data (health or patient details, for example); normal otherwise.", "normal", "phi"),
						"aliases":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Other names; entity_edit adds them."},
						"merge_into":  str("entity_edit: the entity this one duplicates and should be folded into."),
					},
					"required": []string{"name"},
				},
				"unfiled_reason": enum("unfile: why it could not be stored.", "rejected", "no_kind_fits", "no_subject", "unsure", "other"),
				"error":          str("unfile: the rejection message, when reason is rejected."),
				"summary":        str("unfile: one line saying what this was meant to capture."),
				"content":        str("unfile: the information itself."),
			},
			Required: []string{"op"},
		},
	}}}
}

func RecordHandlers() map[string]Handler { return RecordHandlersAs("main", llm.Model) }

// RecordHandlersAs records every write as made by the named agent on the given model.
func RecordHandlersAs(agentName string, model func() string) map[string]Handler {
	if records.Path() == "" {
		return nil
	}
	return map[string]Handler{"vault_record": func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
		return recordAs(ctx, call, cb, agentName, model())
	}}
}

func runRecord(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	return recordAs(ctx, call, cb, "main", llm.Model())
}

type recordSessionKey struct{}

// WithRecordSession names the alux session every record write under ctx is attributed to.
func WithRecordSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, recordSessionKey{}, id)
}

func recordSessionOf(ctx context.Context) string {
	id, _ := ctx.Value(recordSessionKey{}).(string)
	return id
}

// Append-only and local, with refusals kept, so no approval prompt.
func recordAs(ctx context.Context, call Call, cb Callbacks, agentName, model string) anthropic.ContentBlockParamUnion {
	var in struct {
		Op string `json:"op"`
		records.RecordInput
		Record        string               `json:"record"`
		Slot          string               `json:"slot"`
		Event         string               `json:"event"`
		Reason        string               `json:"reason"`
		Entity        *records.EntityInput `json:"entity"`
		UnfiledReason string               `json:"unfiled_reason"`
		Error         string               `json:"error"`
		Summary       string               `json:"summary"`
		Unfiled       int64                `json:"unfiled"`
		Outcome       string               `json:"outcome"`
		Other         string               `json:"other"`
		Verdict       string               `json:"verdict"`
		Session       string               `json:"session"`
		Gist          records.Gist         `json:"gist"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	label := recordLabel(in.Op, in.Kind, in.Subject, in.Title, in.Record, in.Event, in.Entity, in.Summary)
	if in.Op == "resolve" {
		label = fmt.Sprintf("records: resolve · unfiled #%d · %s", in.Unfiled, in.Outcome)
	}
	if in.Op == "judge" {
		label = fmt.Sprintf("records: judge · %s · %s · %s", in.Record, in.Other, in.Verdict)
	}
	cb.toolStart(call.ID, label, KindVault)

	// An add shows the pane the record as written; the model already holds it in its own input.
	show := func(output string) {
		if in.Op != "add" {
			cb.toolResult(call.ID, label, output, KindVault)
			return
		}
		cb.toolResult(call.ID, label, writtenMarkdown(in.RecordInput)+"\n\n---\n\n"+keepLines(output), KindRecord)
	}
	finish := func(output string, err error) anthropic.ContentBlockParamUnion {
		if err != nil {
			show(err.Error())
			return failed(call.ID, err)
		}
		show(output)
		return ok(call.ID, output)
	}

	store, err := openRecordStore()
	if err != nil {
		return finish("", fmt.Errorf("record store unavailable: %w", err))
	}
	actor := records.Actor{Kind: "agent", Agent: agentName, Model: model, Session: recordSessionOf(ctx)}

	switch in.Op {
	case "add":
		res, err := store.Add(in.RecordInput, actor)
		if err != nil {
			return finish("", err)
		}
		out := "Stored " + res.ID + "."
		for _, n := range res.Notes {
			out += "\nNote: " + n + "."
		}
		if len(in.Supersedes) > 0 {
			out += supersessionReport(in.Supersedes, res)
		}
		return finish(out+similarReport(res.Similar, false), nil)
	case "slot":
		slot, err := store.SetSlot(in.Record, in.Slot, actor)
		if err != nil {
			return finish("", err)
		}
		return finish(fmt.Sprintf("%s now fills slot %q.", in.Record, slot), nil)
	case "link":
		res, err := store.Link(in.Record, in.Links, actor)
		if err != nil {
			return finish("", err)
		}
		out := fmt.Sprintf("Added %d link(s) to %s.", res.Added, in.Record)
		if res.AlreadyLinked > 0 {
			out += fmt.Sprintf(" %d already existed and were skipped.", res.AlreadyLinked)
		}
		return finish(out, nil)
	case "cite":
		res, err := store.Cite(in.Record, in.Source, actor)
		if err != nil {
			return finish("", err)
		}
		if !res.Added {
			return finish(fmt.Sprintf("%s already cites that source; still %d distinct source(s).", in.Record, res.Sources), nil)
		}
		return finish(fmt.Sprintf("Cited on %s; %d distinct sources now back it.", in.Record, res.Sources), nil)
	case "event":
		if err := store.Event(in.Record, in.Event, in.Reason, actor); err != nil {
			return finish("", err)
		}
		return finish(fmt.Sprintf("Marked %s %s.", in.Record, in.Event), nil)
	case "entity":
		if in.Entity == nil || in.Entity.Type == "" || in.Entity.Sensitivity == "" {
			return finish("", errors.New("op entity needs an entity object with type, name and sensitivity"))
		}
		res, err := store.AddEntity(*in.Entity, actor)
		if err != nil {
			return finish("", err)
		}
		return finish(fmt.Sprintf("Created entity %s (%s).", in.Entity.Name, res.ID)+similarReport(res.Similar, true), nil)
	case "entity_edit":
		var edit struct {
			Entity *records.EntityEdit `json:"entity"`
		}
		if err := json.Unmarshal(call.Input, &edit); err != nil || edit.Entity == nil {
			return finish("", errors.New("op entity_edit needs an entity object naming the entity to change"))
		}
		res, err := store.EditEntity(*edit.Entity, in.Reason, actor)
		if err != nil {
			return finish("", err)
		}
		return finish(entityEditReport(res), nil)
	case "unfile":
		reason := in.UnfiledReason
		if reason == "" {
			reason = "other"
		}
		id, err := store.Unfile(reason, in.Error, call.Input, in.Summary, in.Sensitivity, actor)
		if err != nil {
			return finish("", err)
		}
		return finish(fmt.Sprintf("Kept as unfiled #%d for review.", id), nil)
	case "resolve":
		if err := store.ResolveUnfiled(in.Unfiled, in.Outcome, in.Record, in.Reason, actor); err != nil {
			return finish("", err)
		}
		return finish(fmt.Sprintf("Resolved unfiled #%d as %s.", in.Unfiled, in.Outcome), nil)
	case "episode":
		if in.Session == recordSessionOf(ctx) || registry.RunningSessions()[in.Session] {
			return finish("", fmt.Errorf("session %s is still running; gist it after it exits", in.Session))
		}
		if err := store.WriteEpisode(records.EpisodeInput{Session: in.Session, Title: in.Title, Gist: in.Gist, Sensitivity: in.Sensitivity}, actor); err != nil {
			return finish("", err)
		}
		return finish(fmt.Sprintf("Gisted session %s; its records now point at it.", in.Session), nil)
	case "judge":
		if err := store.Judge(in.Record, in.Other, in.Verdict, in.Reason, actor); err != nil {
			return finish("", err)
		}
		if in.Verdict == "reopened" {
			return finish(fmt.Sprintf("Reopened %s and %s; the pair is back in the queue.", in.Record, in.Other), nil)
		}
		return finish(fmt.Sprintf("Judged %s and %s %s; the pair leaves the queue.", in.Record, in.Other, in.Verdict), nil)
	}
	return finish("", fmt.Errorf("unknown op %q: use add, link, cite, slot, event, entity, entity_edit, unfile, resolve, judge or episode", in.Op))
}

func entityEditReport(res records.EntityEditResult) string {
	var parts []string
	if res.MergedFrom != "" {
		parts = append(parts, fmt.Sprintf("Merged %s into %s; its names, records and links now answer to %s.", res.MergedFrom, res.ID, res.ID))
	}
	if len(res.AddedAliases) > 0 {
		parts = append(parts, fmt.Sprintf("Added alias(es) to %s: %s.", res.ID, strings.Join(res.AddedAliases, ", ")))
	}
	if len(res.KnownAliases) > 0 {
		parts = append(parts, fmt.Sprintf("Already named %s, skipped: %s.", res.ID, strings.Join(res.KnownAliases, ", ")))
	}
	if res.Described {
		parts = append(parts, fmt.Sprintf("Replaced the description of %s.", res.ID))
	}
	return strings.Join(parts, " ")
}

func supersessionReport(replaced []string, res records.Result) string {
	old := strings.Join(replaced, " and ")
	section := func(head string, labels []string) string {
		if len(labels) == 0 {
			return ""
		}
		return "\n" + head + ": " + strings.Join(labels, "; ") + "."
	}
	cited := section("Cited sources not carried; re-cite with op cite any that still back the new content", res.UncitedSources)
	if len(res.Carried)+len(res.Withheld)+len(res.Dropped) == 0 {
		return "\n" + old + " had no links to carry." + cited
	}
	return section("Carried from "+old, res.Carried) +
		section("Dropped as asked", res.Dropped) +
		section("Not carried; re-add with op link any that still hold", res.Withheld) + cited
}

func similarReport(cands []records.Candidate, entities bool) string {
	if len(cands) == 0 {
		return ""
	}
	var b strings.Builder
	if entities {
		b.WriteString("\nSimilar names, a hint only: if one is the same thing, fold yours into it with entity_edit merge_into.")
	} else {
		b.WriteString("\nSimilar records, a hint only: op link any that yours backs or depends on; if one states the same fact, write one record superseding both.")
	}
	for _, c := range cands {
		if entities {
			fmt.Fprintf(&b, "\n  %s [%s] %s (%s)", c.ID, c.Kind, c.Title, strings.Join(c.Signals, "; "))
			continue
		}
		fmt.Fprintf(&b, "\n  %s [%s about %s] %s (%s)", c.ID, c.Kind, c.Subject, c.Title, strings.Join(c.Signals, "; "))
	}
	return b.String()
}

func recordLabel(op, kind, subject, title, record, event string, entity *records.EntityInput, summary string) string {
	parts := []string{"records: " + op}
	switch op {
	case "add":
		parts = append(parts, kind, subject, title)
	case "event":
		parts = append(parts, record, event)
	case "link", "cite", "slot":
		parts = append(parts, record)
	case "entity", "entity_edit":
		if entity != nil {
			parts = append(parts, entity.Name)
		}
	case "unfile":
		parts = append(parts, summary)
	}
	parts = slices.DeleteFunc(parts, func(p string) bool { return strings.TrimSpace(p) == "" })
	return strings.Join(parts, " · ")
}
