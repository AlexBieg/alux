package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/paths"
)

func TestRecordToolStoresRejectsAndUnfiles(t *testing.T) {
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	if len(RecordTools()) != 1 || RecordHandlers()["vault_record"] == nil {
		t.Fatal("vault_record not registered with ALUX_RECORDS set")
	}
	call := func(input map[string]any) (string, bool) {
		raw, _ := json.Marshal(input)
		res := runRecord(context.Background(), Call{ID: "c", Name: "vault_record", Input: raw}, Callbacks{})
		tr := res.OfToolResult
		var text string
		for _, c := range tr.Content {
			if c.OfText != nil {
				text += c.OfText.Text
			}
		}
		return text, tr.IsError.Or(false)
	}
	src := map[string]any{"type": "conversation", "ref": "test"}

	if out, isErr := call(map[string]any{"op": "entity", "entity": map[string]any{"type": "system", "name": "alux", "sensitivity": "normal"}}); isErr {
		t.Fatal(out)
	}
	if out, isErr := call(map[string]any{"op": "add", "kind": "claim", "subject": "alux", "title": "Filing defaults to Opus", "sensitivity": "normal", "source": src}); isErr || !strings.HasPrefix(out, "Stored rec_") {
		t.Fatalf("add: %q", out)
	}
	out, isErr := call(map[string]any{"op": "add", "kind": "decision", "subject": "alux", "title": "Filing model", "sensitivity": "normal", "source": src})
	if !isErr || !strings.Contains(out, "attrs.chosen") || !strings.Contains(out, "op unfile") {
		t.Fatalf("rejection: %q", out)
	}
	if out, isErr := call(map[string]any{"op": "add", "kind": "decision", "subject": "alux", "title": "Filing model", "attrs": map[string]any{"chosen": "haiku"}, "sensitivity": "normal", "source": src}); isErr {
		t.Fatalf("corrected retry: %q", out)
	}
	if out, isErr := call(map[string]any{"op": "unfile", "unfiled_reason": "rejected", "error": "a decision requires attrs.chosen", "summary": "decision with no clear choice", "sensitivity": "normal"}); isErr || !strings.Contains(out, "unfiled #1") {
		t.Fatalf("unfile: %q", out)
	}
	if out, isErr := call(map[string]any{"op": "resolve", "unfiled": 1, "outcome": "discarded"}); !isErr || !strings.Contains(out, "needs a reason") {
		t.Fatalf("resolve without reason: %q", out)
	}
	if out, isErr := call(map[string]any{"op": "resolve", "unfiled": 1, "outcome": "discarded", "reason": "the corrected retry stored it"}); isErr || out != "Resolved unfiled #1 as discarded." {
		t.Fatalf("resolve: %q", out)
	}
	raw, _ := json.Marshal(map[string]any{"op": "unfiled"})
	res := runRecordRead(context.Background(), Call{ID: "r", Name: "vault_record_read", Input: raw}, Callbacks{})
	if text := res.OfToolResult.Content[0].OfText.Text; text != "No open unfiled entries." {
		t.Fatalf("resolved entry still listed: %q", text)
	}
}

func TestRecordsPointAtTheGistOfTheSessionThatWroteThem(t *testing.T) {
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	run := func(session string, h Handler, input map[string]any) string {
		raw, _ := json.Marshal(input)
		res := h(WithRecordSession(context.Background(), session), Call{ID: "c", Input: raw}, Callbacks{})
		text := res.OfToolResult.Content[0].OfText.Text
		if res.OfToolResult.IsError.Or(false) {
			t.Fatalf("%v: %s", input, text)
		}
		return text
	}
	add := func(session, title string) string {
		out := run(session, runRecord, map[string]any{"op": "add", "kind": "claim", "subject": "alux", "title": title,
			"sensitivity": "normal", "source": map[string]any{"type": "conversation", "ref": title}})
		return strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "Stored "), ".")
	}
	run("gisted", runRecord, map[string]any{"op": "entity", "entity": map[string]any{"type": "system", "name": "alux", "sensitivity": "normal"}})
	gisted := add("gisted", "Written in a session that was gisted")
	add("bare", "Written in a session with no gist")
	run("", runRecord, map[string]any{"op": "episode", "session": "gisted", "title": "Built the probe", "sensitivity": "normal",
		"gist": map[string]any{"goal": "probe recall", "outcome": "25% hit@5", "surprises": []string{"vectors already unit-norm"}}})

	if out := run("", runRecordRead, map[string]any{"op": "get", "id": gisted}); !strings.Contains(out, "episode: Built the probe (session gisted)") {
		t.Errorf("get lacks the gist pointer: %q", out)
	}
	about := run("", runRecordRead, map[string]any{"op": "about", "subject": "alux"})
	if strings.Count(about, "episode:") != 1 {
		t.Errorf("about should point only the gisted record at a gist: %q", about)
	}
	ep := run("", runRecordRead, map[string]any{"op": "episode", "session": "gisted"})
	if !strings.Contains(ep, "surprise: vectors already unit-norm") || !strings.Contains(ep, gisted) {
		t.Errorf("episode read lacks the gist or its records: %q", ep)
	}
}

func TestUngistedListsOnlyExitedSessionsWithATranscriptOtherThanTheReader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	run := func(session string, h Handler, input map[string]any) string {
		raw, _ := json.Marshal(input)
		res := h(WithRecordSession(context.Background(), session), Call{ID: "c", Input: raw}, Callbacks{})
		text := res.OfToolResult.Content[0].OfText.Text
		if res.OfToolResult.IsError.Or(false) {
			t.Fatalf("%v: %s", input, text)
		}
		return text
	}
	run("", runRecord, map[string]any{"op": "entity", "entity": map[string]any{"type": "system", "name": "alux", "sensitivity": "normal"}})
	dir := SessionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"ended", "running", "gisted", "reader", "pruned"} {
		run(s, runRecord, map[string]any{"op": "add", "kind": "claim", "subject": "alux", "title": "Written in " + s,
			"sensitivity": "normal", "source": map[string]any{"type": "conversation", "ref": s}})
		if s == "pruned" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, s+".jsonl"), []byte(`{"kind":"user","text":"hi"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	live := filepath.Join(home, paths.Cache, "sessions")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := fmt.Sprintf(`{"pid":%d,"session":"running"}`, os.Getppid())
	if err := os.WriteFile(filepath.Join(live, strconv.Itoa(os.Getppid())+".json"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	run("", runRecord, map[string]any{"op": "episode", "session": "gisted", "title": "Already gisted", "sensitivity": "normal",
		"gist": map[string]any{"goal": "g", "outcome": "o"}})

	for _, c := range []struct{ caller, target string }{{"reader", "running"}, {"reader", "reader"}} {
		raw, _ := json.Marshal(map[string]any{"op": "episode", "session": c.target, "title": "Too early", "sensitivity": "normal",
			"gist": map[string]any{"goal": "g", "outcome": "o"}})
		res := runRecord(WithRecordSession(context.Background(), c.caller), Call{ID: "c", Input: raw}, Callbacks{})
		if !res.OfToolResult.IsError.Or(false) {
			t.Errorf("gisting %s from %s should be refused while it runs", c.target, c.caller)
		}
	}

	out := run("reader", runRecordRead, map[string]any{"op": "ungisted"})
	if !strings.Contains(out, "ended (ended ") || !strings.Contains(out, filepath.Join(dir, "ended.jsonl")) {
		t.Errorf("an ended session with its transcript should be listed with its path: %q", out)
	}
	for _, s := range []string{"running", "gisted", "reader", "pruned"} {
		if strings.Contains(out, s+" (ended") {
			t.Errorf("%s should not be listed: %q", s, out)
		}
	}
}

func TestRecordReadToolSearchesGetsAndListsAbout(t *testing.T) {
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	run := func(h Handler, name string, input map[string]any) string {
		raw, _ := json.Marshal(input)
		res := h(context.Background(), Call{ID: "c", Name: name, Input: raw}, Callbacks{})
		var text string
		for _, c := range res.OfToolResult.Content {
			if c.OfText != nil {
				text += c.OfText.Text
			}
		}
		if res.OfToolResult.IsError.Or(false) {
			t.Fatalf("%s %v: %s", name, input, text)
		}
		return text
	}
	src := map[string]any{"type": "conversation", "ref": "test"}
	run(runRecord, "vault_record", map[string]any{"op": "entity", "entity": map[string]any{"type": "system", "name": "alux", "sensitivity": "normal", "aliases": []string{"the alux tui"}}})
	stored := run(runRecord, "vault_record", map[string]any{"op": "add", "kind": "claim", "subject": "alux", "title": "Filing defaults to Opus", "attrs": map[string]any{"workaround": "set ALUX_VAULT_MODEL"}, "sensitivity": "normal", "source": src})
	id := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(stored, "\n", 2)[0], "Stored "), ".")

	if out := run(runRecordRead, "vault_record_read", map[string]any{"op": "search", "query": "which model does filing use"}); !strings.Contains(out, id) || strings.Contains(out, "ALUX_VAULT_MODEL") {
		t.Fatalf("search: %q", out)
	}
	if out := run(runRecordRead, "vault_record_read", map[string]any{"op": "get", "id": id}); !strings.Contains(out, "source: conversation: test") || !strings.Contains(out, "recorded:") || !strings.Contains(out, "ALUX_VAULT_MODEL") {
		t.Fatalf("get: %q", out)
	}
	if out := run(runRecordRead, "vault_record_read", map[string]any{"op": "about", "subject": "the alux tui"}); !strings.Contains(out, "## claim") || !strings.Contains(out, "aliases: the alux tui") {
		t.Fatalf("about: %q", out)
	}
	if out := run(runRecordRead, "vault_record_read", map[string]any{"op": "unfiled"}); out != "No open unfiled entries." {
		t.Fatalf("unfiled: %q", out)
	}
}

func TestRecordToolsShowThePaneMoreThanTheModelGets(t *testing.T) {
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	run := func(h Handler, name string, input map[string]any) (model, pane string) {
		raw, _ := json.Marshal(input)
		cb := Callbacks{OnToolResult: func(_, _, output string, _ ToolKind) { pane = output }}
		res := h(context.Background(), Call{ID: "c", Name: name, Input: raw}, cb)
		for _, c := range res.OfToolResult.Content {
			if c.OfText != nil {
				model += c.OfText.Text
			}
		}
		if res.OfToolResult.IsError.Or(false) {
			t.Fatalf("%s %v: %s", name, input, model)
		}
		return model, pane
	}
	run(runRecord, "vault_record", map[string]any{"op": "entity", "entity": map[string]any{"type": "system", "name": "alux", "sensitivity": "normal"}})
	model, pane := run(runRecord, "vault_record", map[string]any{"op": "add", "kind": "procedure", "subject": "alux", "title": "Release the binary",
		"text": "why it ships this way", "body": "BODYPROSE in full", "attrs": map[string]any{"steps": []string{"STEPONE", "STEPTWO"}},
		"sensitivity": "normal", "source": map[string]any{"type": "conversation", "ref": "test"}})
	for _, want := range []string{"Release the binary", "why it ships this way", "BODYPROSE", "STEPONE", "STEPTWO", "Stored rec_"} {
		if !strings.Contains(pane, want) {
			t.Errorf("add pane lacks %q:\n%s", want, pane)
		}
	}
	if strings.Contains(model, "BODYPROSE") || strings.Contains(model, "STEPONE") {
		t.Errorf("add echoed the record back to the model: %q", model)
	}

	model, pane = run(runRecordRead, "vault_record_read", map[string]any{"op": "search", "query": "release binary"})
	if !strings.Contains(pane, "### Release the binary") || !strings.Contains(pane, "STEPONE") {
		t.Errorf("search pane is not the markdown view:\n%s", pane)
	}
	if strings.Contains(model, "###") || strings.Contains(model, "STEPONE") {
		t.Errorf("search sent the pane's view to the model: %q", model)
	}
}

func TestRecordReadToolKeepsLargeEntitiesCheapAndMarksPartialSearches(t *testing.T) {
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	run := func(h Handler, name string, input map[string]any) string {
		raw, _ := json.Marshal(input)
		res := h(context.Background(), Call{ID: "c", Name: name, Input: raw}, Callbacks{})
		var text string
		for _, c := range res.OfToolResult.Content {
			if c.OfText != nil {
				text += c.OfText.Text
			}
		}
		if res.OfToolResult.IsError.Or(false) {
			t.Fatalf("%s %v: %s", name, input, text)
		}
		return text
	}
	src := map[string]any{"type": "conversation", "ref": "test"}
	run(runRecord, "vault_record", map[string]any{"op": "entity", "entity": map[string]any{"type": "project", "name": "Big Project", "sensitivity": "normal"}})
	for i := range aboutFullMax {
		run(runRecord, "vault_record", map[string]any{"op": "add", "kind": "claim", "subject": "Big Project", "title": fmt.Sprintf("Claim number %d", i),
			"attrs": map[string]any{"consequence": "long detail that only a full read shows"}, "sensitivity": "normal", "source": src})
	}
	run(runRecord, "vault_record", map[string]any{"op": "add", "kind": "todo", "subject": "Big Project", "title": "Ship it", "text": "why it ships", "sensitivity": "normal", "source": src})

	out := run(runRecordRead, "vault_record_read", map[string]any{"op": "about", "subject": "Big Project"})
	if !strings.Contains(out, "titles only") || strings.Contains(out, "long detail") || !strings.Contains(out, "## claim (25)") || !strings.Contains(out, "Ship it") {
		t.Fatalf("large about: %q", out)
	}
	out = run(runRecordRead, "vault_record_read", map[string]any{"op": "about", "subject": "Big Project", "kind": "todo"})
	if strings.Contains(out, "titles only") || strings.Contains(out, "Claim number") || !strings.Contains(out, "Ship it") {
		t.Fatalf("about by kind: %q", out)
	}

	out = run(runRecordRead, "vault_record_read", map[string]any{"op": "search", "query": "ship zebra"})
	if !strings.HasPrefix(out, "No record matches every word") || !strings.Contains(out, "Ship it") || strings.Contains(out, "why it ships") {
		t.Fatalf("partial search: %q", out)
	}
}

func TestRecordToolEditsAndMergesEntities(t *testing.T) {
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	call := func(input map[string]any) (string, bool) {
		raw, _ := json.Marshal(input)
		res := runRecord(context.Background(), Call{ID: "c", Name: "vault_record", Input: raw}, Callbacks{})
		var text string
		for _, c := range res.OfToolResult.Content {
			if c.OfText != nil {
				text += c.OfText.Text
			}
		}
		return text, res.OfToolResult.IsError.Or(false)
	}
	first, isErr := call(map[string]any{"op": "entity", "entity": map[string]any{"type": "project", "name": "Profile Improvements", "sensitivity": "normal"}})
	if isErr {
		t.Fatal(first)
	}
	firstID := regexp.MustCompile(`ent_[0-9a-f]+`).FindString(first)
	draft := map[string]any{"op": "entity", "entity": map[string]any{"type": "project", "name": "Profile Optimization", "sensitivity": "normal"}}
	if out, isErr := call(draft); isErr || !strings.Contains(out, "Created entity Profile Optimization") || !strings.Contains(out, "Similar names") || !strings.Contains(out, firstID) {
		t.Fatalf("a near-name entity was not created with the close name as a hint: %q", out)
	}
	if out, isErr := call(map[string]any{"op": "entity", "entity": map[string]any{"name": "No Type"}}); !isErr || !strings.Contains(out, "type, name and sensitivity") {
		t.Fatalf("create without type: %q", out)
	}
	out, isErr := call(map[string]any{"op": "entity_edit", "reason": "one project", "entity": map[string]any{
		"name": "Profile Optimization", "merge_into": "Profile Improvements", "aliases": []string{"bio eval"}}})
	if isErr || !strings.Contains(out, "Merged") || !strings.Contains(out, "bio eval") {
		t.Fatalf("merge: %q", out)
	}
	if out, isErr := call(map[string]any{"op": "add", "kind": "claim", "subject": "bio eval", "title": "Lands under the remaining entity", "sensitivity": "normal",
		"source": map[string]any{"type": "conversation", "ref": "test"}}); isErr {
		t.Fatalf("add via new alias: %q", out)
	}
}

func TestRecordToolsAreAbsentWhenUnconfigured(t *testing.T) {
	for _, off := range []string{"0", "false", "off"} {
		t.Setenv("ALUX_RECORDS", off)
		if RecordTools() != nil || RecordHandlers() != nil || RecordReadTools() != nil || RecordReadHandlers() != nil || RecordSystemNote() != "" {
			t.Fatalf("record tools registered with ALUX_RECORDS=%s", off)
		}
	}
	for _, on := range []string{"", "1", "true"} {
		t.Setenv("ALUX_RECORDS", on)
		if RecordTools() == nil || !strings.Contains(RecordSystemNote(), "store it with `vault_record`") {
			t.Fatalf("record tools missing with ALUX_RECORDS=%q", on)
		}
	}
}
