package records

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/taint"
)

func reading(title, asOf, slot string) RecordInput {
	attrs := map[string]any{"value": title, "as_of": asOf}
	if slot != "" {
		attrs["slot"] = slot
	}
	in := rec("state", title, attrs)
	in.ValidFrom = asOf
	return in
}

func TestAMergeKeepsTheOlderReadingForItsOwnDates(t *testing.T) {
	s := open(t)
	olderIn := reading("PR head aaaa1111", "2026-10-14", "")
	olderIn.Source = Source{Type: "file", Ref: "older.md"}
	olderIn.Links = []Link{{Role: "about", Entity: "Sam"}}
	older, err := s.Add(olderIn, agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cite(older.ID, Source{Type: "url", Ref: "https://example.com/pr"}, agent); err != nil {
		t.Fatal(err)
	}
	claim, _ := s.Add(rec("claim", "Design export adopted", nil), agent)
	newerIn := reading("PR head bbbb2222", "2026-10-15", "")
	newerIn.Links = []Link{{Role: "see_also", Record: claim.ID}}
	newer := write(t, s, newerIn)

	mergedIn := reading("PR head bbbb2222", "2026-10-15", "pr-heads")
	mergedIn.Source = Source{Type: "conversation", Ref: "merge session"}
	mergedIn.Supersedes = IDs{newer.ID, older.ID}
	res, err := s.Add(mergedIn, agent)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Carried) != 2 {
		t.Fatalf("links of both predecessors should carry: %v", res.Carried)
	}
	uncited := strings.Join(res.UncitedSources, " | ")
	if !strings.Contains(uncited, "https://example.com/pr") || !strings.Contains(uncited, "file: older.md") || strings.Contains(uncited, "test session") {
		t.Fatalf("uncited should name the older record's citation and own source, not the first's: %v", res.UncitedSources)
	}

	then, err := s.AboutAt("subagents", "state", AsOf{ValidAt: "2026-10-14"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := ids(then.Records); len(ids) != 1 || ids[0] != older.ID {
		t.Fatalf("as of 2026-10-14 the older reading held: %v", ids)
	}
	now, _ := s.About("subagents", "state")
	if ids := ids(now.Records); len(ids) != 1 || ids[0] != res.ID {
		t.Fatalf("today only the merged record holds: %v", ids)
	}
	for _, id := range []string{older.ID, newer.ID} {
		if d, _ := s.Get(id); d.Status != "superseded" || !strings.HasPrefix(d.Successor, res.ID) {
			t.Errorf("%s: status %s, successor %q", id, d.Status, d.Successor)
		}
	}
	if d, _ := s.Get(res.ID); len(d.Predecessors) != 2 {
		t.Fatalf("both predecessors should show: %v", d.Predecessors)
	}

	again := reading("PR head cccc3333", "2026-10-16", "")
	again.Supersedes = IDs{older.ID}
	rejected(t, second(s, again), "replaced by "+res.ID)
}

func TestALaterVersionInEffectRetiresTheWholeChain(t *testing.T) {
	s := open(t)
	a, _ := s.Add(reading("Canary at 5%", "2026-03-21", "allocation"), agent)
	futureIn := reading("Canary at 100%", "2099-01-01", "allocation")
	futureIn.Supersedes = IDs{a.ID}
	future, err := s.Add(futureIn, agent)
	if err != nil {
		t.Fatal(err)
	}
	if now, _ := s.About("subagents", "state"); len(now.Records) != 1 || now.Records[0].ID != a.ID {
		t.Fatalf("before the future version takes effect the old one holds: %v", ids(now.Records))
	}

	fixIn := reading("Canary at 60%", "2026-04-19", "allocation")
	fixIn.ValidFrom = ""
	fixIn.Supersedes = IDs{future.ID}
	fix, err := s.Add(fixIn, agent)
	if err != nil {
		t.Fatalf("a future-dated holder should not block superseding it: %v", err)
	}
	now, _ := s.About("subagents", "state")
	if len(now.Records) != 1 || now.Records[0].ID != fix.ID {
		t.Fatalf("an undated version two steps on should retire the first too: %v", ids(now.Records))
	}
	if then, _ := s.AboutAt("subagents", "state", AsOf{ValidAt: "2026-03-31"}); len(then.Records) != 1 || then.Records[0].ID != fix.ID {
		t.Fatalf("an undated latest version replaces the chain on every date: %v", ids(then.Records))
	}
}

func TestAMergeCarriesNoLinkToItselfOrItsPredecessors(t *testing.T) {
	s := open(t)
	z, _ := s.Add(rec("claim", "Z first wording", nil), agent)
	aIn := rec("claim", "A links Z", nil)
	aIn.Links = []Link{{Role: "see_also", Record: z.ID}}
	a, _ := s.Add(aIn, agent)
	bIn := rec("claim", "B replaces Z", nil)
	bIn.Supersedes = IDs{z.ID}
	b, _ := s.Add(bIn, agent)
	merged := rec("claim", "A and B merged", nil)
	merged.Supersedes = IDs{a.ID, b.ID}
	res, err := s.Add(merged, agent)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM links WHERE from_record = ?1 AND to_record = ?1`, res.ID); n != 0 {
		t.Fatalf("merge wrote %d self-links", n)
	}
}

func TestAColumnOnlySupersessionStillCounts(t *testing.T) {
	s := open(t)
	old, _ := s.Add(rec("claim", "Filing defaults to Opus", nil), agent)
	if _, err := s.db.Exec(`INSERT INTO txns (id, actor) VALUES ('tx_old', 'agent')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO records (id, kind, subject_id, title, supersedes, sensitivity, derived_from, tx_id)
		SELECT 'rec_old', kind, subject_id, 'Filing defaults to Haiku', id, sensitivity, derived_from, 'tx_old' FROM records WHERE id = ?`, old.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(old.ID); d.Status != "superseded" || !strings.HasPrefix(d.Successor, "rec_old") {
		t.Fatalf("a column-only supersession should read as replaced: %s %q", d.Status, d.Successor)
	}
}

func TestAMergeAcrossKindsWritesNothing(t *testing.T) {
	s := open(t)
	st, _ := s.Add(reading("Canary at 20%", "2026-04-12", ""), agent)
	cl, _ := s.Add(rec("claim", "Canary never reached 60%", nil), agent)
	before := count(t, s, `SELECT count(*) FROM records`)
	mixed := reading("Canary at 60%", "2026-04-19", "")
	mixed.Supersedes = IDs{st.ID, cl.ID}
	rejected(t, second(s, mixed), "same kind")
	if n := count(t, s, `SELECT count(*) FROM records`); n != before {
		t.Fatalf("a refused merge wrote %d records", n-before)
	}
	if d, _ := s.Get(st.ID); d.Status != "active" {
		t.Fatalf("a refused merge replaced %s", st.ID)
	}
}

func TestSupersedesTakesOneIDOrAList(t *testing.T) {
	for raw, want := range map[string][]string{
		`{"supersedes": "rec_a"}`:            {"rec_a"},
		`{"supersedes": ["rec_a", "rec_b"]}`: {"rec_a", "rec_b"},
		`{"supersedes": ""}`:                 nil,
		`{}`:                                 nil,
	} {
		var in RecordInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !slices.Equal([]string(in.Supersedes), want) {
			t.Errorf("%s: got %v, want %v", raw, in.Supersedes, want)
		}
	}
}

func TestASlotSetInPlaceKeepsTheRecord(t *testing.T) {
	s := open(t)
	firstIn := reading("Reading one", "2026-10-14", "")
	firstIn.Links = []Link{{Role: "about", Entity: "Sam"}}
	first, _ := s.Add(firstIn, agent)
	if _, err := s.Cite(first.ID, Source{Type: "url", Ref: "https://example.com/ci"}, agent); err != nil {
		t.Fatal(err)
	}

	slot, err := s.SetSlot(first.ID, " PR-Heads ", agent)
	if err != nil || slot != "pr-heads" {
		t.Fatalf("set slot: %q %v", slot, err)
	}
	d, _ := s.Get(first.ID)
	if d.Status != "active" || d.Slot != "pr-heads" || !strings.Contains(d.Attrs, `"slot":"pr-heads"`) || d.Sources != 2 || len(d.Links) != 1 {
		t.Fatalf("the record should keep its id, links and citation and show the slot: %+v", d)
	}
	if got, _ := s.Search(SearchInput{Query: "pr-heads"}); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("a slot set in place should be searchable: %v", ids(got))
	}

	_, err = s.SetSlot(first.ID, "allocation", agent)
	rejected(t, err, "set once")
	other, _ := s.Add(reading("Reading two", "2026-10-15", ""), agent)
	_, err = s.SetSlot(other.ID, "pr-heads", agent)
	rejected(t, err, first.ID)
	rejected(t, second(s, reading("Reading three", "2026-10-16", "pr-heads")), first.ID)
	cl, _ := s.Add(rec("claim", "Not a state", nil), agent)
	_, err = s.SetSlot(cl.ID, "pr-heads", agent)
	rejected(t, err, "only a state")
	written, _ := s.Add(reading("Reading four", "2026-10-16", "ci"), agent)
	_, err = s.SetSlot(written.ID, "other", agent)
	rejected(t, err, "set once")
}

func TestConflictsListsOverlappingStatesAndOnlyCountsPHI(t *testing.T) {
	s := open(t)
	a, _ := s.Add(reading("Canary at 20%", "2026-04-12", ""), agent)
	b, _ := s.Add(reading("Canary attached", "2026-03-22", ""), agent)
	for _, slot := range []string{"main-bookmark", "next-step"} {
		in := reading("Sam "+slot, "2026-10-14", slot)
		in.Subject = "Sam"
		if _, err := s.Add(in, agent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddEntity(EntityInput{Type: "topic", Name: "Patient intake", Sensitivity: "phi"}, agent); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Intake form sent", "Intake form returned"} {
		in := reading(title, "2026-10-14", "")
		in.Subject, in.Sensitivity = "Patient intake", "phi"
		if _, err := s.Add(in, agent); err != nil {
			t.Fatal(err)
		}
	}

	taint.ResetForTest()
	t.Cleanup(taint.ResetForTest)
	cs, phi, err := s.Conflicts()
	if err != nil {
		t.Fatal(err)
	}
	if taint.Tainted() {
		t.Fatal("the conflicts read tainted the session")
	}
	if phi != 1 || len(cs) != 1 || cs[0].Subject != "Agent Supervisor" || len(cs[0].States) != 2 {
		t.Fatalf("want subagents listed and one phi subject counted: phi=%d %+v", phi, cs)
	}

	for id, slot := range map[string]string{a.ID: "allocation", b.ID: "canary"} {
		if _, err := s.SetSlot(id, slot, agent); err != nil {
			t.Fatal(err)
		}
	}
	if cs, _, _ := s.Conflicts(); len(cs) != 0 {
		t.Fatalf("distinct slots should clear the subject: %+v", cs)
	}
}

func TestConflictsCatchesASlotSharedAfterAnEntityMerge(t *testing.T) {
	s := open(t)
	entity(t, s, EntityInput{Type: "system", Name: "Subagent Runner", Sensitivity: "normal"})
	if _, err := s.Add(reading("Main at aaaabbbb", "2026-10-14", "main-bookmark"), agent); err != nil {
		t.Fatal(err)
	}
	dup := reading("Main at ccccdddd", "2026-10-13", "main-bookmark")
	dup.Subject = "Subagent Runner"
	if _, err := s.Add(dup, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditEntity(EntityEdit{Name: "Subagent Runner", MergeInto: "subagents"}, "duplicate", agent); err != nil {
		t.Fatal(err)
	}
	cs, _, err := s.Conflicts()
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || !slices.Equal(cs[0].Shared, []string{"main-bookmark"}) {
		t.Fatalf("a merge that puts two holders in one slot should show: %+v", cs)
	}
}

func TestBacklogCountsWorkableConflictsAndOpenUnfiledWithoutTainting(t *testing.T) {
	s := open(t)
	if c, u, err := s.Backlog(); err != nil || c != 0 || u != 0 {
		t.Fatalf("an empty store has a backlog: conflicts=%d unfiled=%d err=%v", c, u, err)
	}
	if _, err := s.AddEntity(EntityInput{Type: "topic", Name: "Patient intake", Sensitivity: "phi"}, agent); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Intake form sent", "Intake form returned"} {
		in := reading(title, "2026-10-14", "")
		in.Subject, in.Sensitivity = "Patient intake", "phi"
		if _, err := s.Add(in, agent); err != nil {
			t.Fatal(err)
		}
	}
	if c, _, _ := s.Backlog(); c != 0 {
		t.Fatalf("a phi-only overlap is not cleanup work, got %d conflicts", c)
	}
	s.Add(reading("Canary at 20%", "2026-04-12", ""), agent)
	s.Add(reading("Canary attached", "2026-03-22", ""), agent)
	if _, err := s.Unfile("no_kind_fits", "", []byte(`{"text":"a detail"}`), "a detail", "phi", agent); err != nil {
		t.Fatal(err)
	}
	taint.ResetForTest()
	t.Cleanup(taint.ResetForTest)
	c, u, err := s.Backlog()
	if err != nil || c != 1 || u != 1 {
		t.Fatalf("want one conflict and one unfiled entry: conflicts=%d unfiled=%d err=%v", c, u, err)
	}
	if taint.Tainted() {
		t.Fatal("counting the backlog tainted the session")
	}
}
