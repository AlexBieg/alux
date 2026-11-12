package records

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var agent = Actor{Kind: "agent", Agent: "test", Model: "test-model"}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.AddEntity(EntityInput{Type: "system", Name: "Agent Supervisor", Sensitivity: "normal", Aliases: []string{"subagents"}}, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEntity(EntityInput{Type: "person", Name: "Sam", Sensitivity: "normal"}, agent); err != nil {
		t.Fatal(err)
	}
	return s
}

func write(t *testing.T, s *Store, in RecordInput) Result {
	t.Helper()
	res, err := s.Add(in, agent)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func entity(t *testing.T, s *Store, in EntityInput) EntityResult {
	t.Helper()
	res, err := s.AddEntity(in, agent)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func rec(kind, title string, attrs map[string]any) RecordInput {
	return RecordInput{
		Kind: kind, Subject: "subagents", Title: title, Attrs: attrs, Sensitivity: "normal",
		Source: Source{Type: "conversation", Ref: "test session"},
	}
}

func rejected(t *testing.T, err error, want string) *Rejection {
	t.Helper()
	var r *Rejection
	if !errors.As(err, &r) {
		t.Fatalf("want a rejection containing %q, got %v", want, err)
	}
	if !strings.Contains(r.Message, want) {
		t.Fatalf("rejection %q does not mention %q", r.Message, want)
	}
	return r
}

func TestAddWithoutSensitivitySaysItIsRequired(t *testing.T) {
	s := open(t)
	in := rec("claim", "Runs detached by default", nil)
	in.Sensitivity = ""
	_, err := s.Add(in, agent)
	rejected(t, err, "sensitivity is required on every add")
	if n := count(t, s, `SELECT count(*) FROM records`); n != 0 {
		t.Fatalf("a refused add wrote %d record(s)", n)
	}
}

func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAddResolvesSubjectByAliasAndEntityAttrsToIDs(t *testing.T) {
	s := open(t)
	res, err := s.Add(rec("todo", "Point filing at Haiku", map[string]any{"owner": "sam"}), agent)
	if err != nil {
		t.Fatal(err)
	}
	var subjectName, owner string
	if err := s.db.QueryRow(`SELECT e.name, json_extract(r.attrs, '$.owner') FROM records r JOIN entities e ON e.id = r.subject_id WHERE r.id = ?`, res.ID).Scan(&subjectName, &owner); err != nil {
		t.Fatal(err)
	}
	if subjectName != "Agent Supervisor" || !strings.HasPrefix(owner, "ent_") {
		t.Fatalf("subject %q, owner %q", subjectName, owner)
	}
}

func TestAboutListsRecordsThatNameTheEntityInAttrsOrLinks(t *testing.T) {
	s := open(t)
	if _, err := s.AddEntity(EntityInput{Type: "team", Name: "Legal", Sensitivity: "normal"}, agent); err != nil {
		t.Fatal(err)
	}
	decision := rec("decision", "Summaries may run through a hosted model", map[string]any{"chosen": "summary text only", "decided_by": "Legal"})
	decision.Links = []Link{{Role: "about", Entity: "Legal"}}
	byLegal, err := s.Add(decision, agent)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := s.Add(rec("todo", "Point filing at Haiku", map[string]any{"owner": "Sam"}), agent)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM links WHERE from_record = ? AND role = 'about'`, byLegal.ID); n != 1 {
		t.Fatalf("decision has %d about links, want 1", n)
	}

	for name, want := range map[string]string{"Legal": byLegal.ID, "Sam": owned.ID} {
		e, err := s.About(name, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(e.Records) != 0 || len(e.Mentions) != 1 || e.Mentions[0].ID != want {
			t.Fatalf("%s: records %v, mentions %v, want only mention %s", name, e.Records, e.Mentions, want)
		}
	}
	subject, err := s.About("subagents", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(subject.Records) != 2 || len(subject.Mentions) != 0 {
		t.Fatalf("subject: records %d, mentions %d", len(subject.Records), len(subject.Mentions))
	}
}

func TestUnknownAttrsAreKeptUnderExtraWithANote(t *testing.T) {
	s := open(t)
	res, err := s.Add(rec("claim", "Filing defaults to Opus", map[string]any{"severity": "high"}), agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "severity") {
		t.Fatalf("notes %v", res.Notes)
	}
	if n := count(t, s, `SELECT count(*) FROM records WHERE json_extract(attrs, '$.extra.severity') = 'high'`); n != 1 {
		t.Fatalf("extra not stored")
	}
}

func TestRejectionsExplainAndWriteNothing(t *testing.T) {
	s := open(t)
	cases := []struct {
		name, want, reason string
		in                 RecordInput
	}{
		{"unknown subject", "unknown subject", "no_subject", func() RecordInput { r := rec("claim", "x claim", nil); r.Subject = "Nope"; return r }()},
		{"missing required attr", "attrs.chosen", "rejected", rec("decision", "Filing model", nil)},
		{"wrong attr type", "attrs.value must be a number", "rejected", rec("measurement", "Filing tokens", map[string]any{"metric": "input", "value": "lots"})},
		{"bad enum", "attrs.priority must be one of", "rejected", rec("todo", "Some todo", map[string]any{"priority": "urgent"})},
		{"body on a claim", "body IS NULL", "rejected", func() RecordInput { r := rec("claim", "Long claim", nil); r.Body = "paragraphs"; return r }()},
		{"no source", "source.type", "rejected", func() RecordInput { r := rec("claim", "Sourceless", nil); r.Source = Source{}; return r }()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Add(c.in, agent)
			r := rejected(t, err, c.want)
			if r.Reason != c.reason {
				t.Fatalf("reason %q, want %q", r.Reason, c.reason)
			}
		})
	}
	if n := count(t, s, `SELECT count(*) FROM records`); n != 0 {
		t.Fatalf("%d records written by rejected adds", n)
	}
	if n := count(t, s, `SELECT count(*) FROM unfiled`); n != 0 {
		t.Fatalf("%d rejections were unfiled without being asked", n)
	}
}

func TestLengthRefusalsGiveTheCountAndMatchTheSchemaLimit(t *testing.T) {
	s := open(t)
	long := func(n int) string { return strings.Repeat("é", n) }
	for _, c := range []struct {
		name, title, text, want string
	}{
		{"text over", "A claim", long(697), "text is 697 characters, 97 over the limit of 600"},
		{"title over", long(203), "", "title is 203 characters, 3 over the limit of 200"},
		{"title short", "ab", "", "title is 2 characters, at least 3 are needed"},
		{"both over", long(201), long(601), "title is 201 characters, 1 over the limit of 200; text is 601 characters, 1 over"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := rec("claim", c.title, nil)
			r.Text = c.text
			_, err := s.Add(r, agent)
			rejected(t, err, c.want)
		})
	}
	r := rec("claim", long(200), nil)
	r.Text = long(600)
	if _, err := s.Add(r, agent); err != nil {
		t.Fatalf("multibyte title and text at exactly the limits refused: %v", err)
	}
}

func TestUnfileRejectedNeedsTheError(t *testing.T) {
	s := open(t)
	if _, err := s.Unfile("rejected", "", []byte(`{}`), "a refused decision", "normal", agent); err == nil {
		t.Fatal("unfiled a rejection with no error text")
	}
	if _, err := s.Unfile("no_subject", "", []byte(`{}`), "no entity fits", "normal", agent); err != nil {
		t.Fatal(err)
	}
}

func TestSupersessionMustKeepKindAndAQuestionIsAnsweredByLink(t *testing.T) {
	s := open(t)
	q, err := s.Add(rec("question", "Should filing use Haiku?", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	d := rec("decision", "Filing uses Haiku", map[string]any{"chosen": "haiku"})
	d.Supersedes = IDs{q.ID}
	_, err = s.Add(d, agent)
	rejected(t, err, "same kind")

	d.Supersedes = nil
	d.Links = []Link{{Role: "answers", Record: q.ID}}
	if _, err := s.Add(d, agent); err != nil {
		t.Fatal(err)
	}
	if err := s.Event(q.ID, "done", "answered", agent); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM record_status WHERE id = ?`, q.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("question status %q", status)
	}
}

func TestSupersessionCarriesTopicalLinksAndWithholdsAssertingOnes(t *testing.T) {
	s := open(t)
	if _, err := s.AddEntity(EntityInput{Type: "team", Name: "Legal", Sensitivity: "normal"}, agent); err != nil {
		t.Fatal(err)
	}
	target, _ := s.Add(rec("claim", "Target v1", nil), agent)
	nextTarget := rec("claim", "Target v2", nil)
	nextTarget.Supersedes = IDs{target.ID}
	target2, _ := s.Add(nextTarget, agent)
	disputed, _ := s.Add(rec("claim", "Disputed", nil), agent)
	gone, _ := s.Add(rec("claim", "Gone", nil), agent)
	unwanted, _ := s.Add(rec("claim", "Unwanted", nil), agent)

	old := rec("claim", "Old wording", nil)
	old.Links = []Link{
		{Role: "refines", Record: target.ID},
		{Role: "see_also", Record: gone.ID},
		{Role: "see_also", Record: unwanted.ID},
		{Role: "about", Entity: "Legal"},
		{Role: "contradicts", Record: disputed.ID},
	}
	v1, err := s.Add(old, agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Event(gone.ID, "retracted", "wrong", agent); err != nil {
		t.Fatal(err)
	}

	next := rec("claim", "New wording", nil)
	next.Supersedes = IDs{v1.ID}
	next.DropLinks = []Link{{Role: "see_also", Record: unwanted.ID}}
	res, err := s.Add(next, agent)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"refines|" + target2.ID: true, "about|Legal": true}
	rows, err := s.db.Query(`SELECT l.role, coalesce(l.to_record, e.name) FROM links l LEFT JOIN entities e ON e.id = l.to_entity WHERE l.from_record = ?`, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for rows.Next() {
		var role, to string
		if err := rows.Scan(&role, &to); err != nil {
			t.Fatal(err)
		}
		got[role+"|"+to] = true
	}
	rows.Close()
	if len(got) != len(want) {
		t.Fatalf("new record's links %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("new record's links %v, want %v", got, want)
		}
	}
	if len(res.Carried) != 2 || len(res.Dropped) != 1 || len(res.Withheld) != 2 {
		t.Fatalf("carried %q, dropped %q, withheld %q", res.Carried, res.Dropped, res.Withheld)
	}
	if w := strings.Join(res.Withheld, "\n"); !strings.Contains(w, "contradicts → "+disputed.ID) || !strings.Contains(w, gone.ID+" (retracted)") {
		t.Fatalf("withheld %q", res.Withheld)
	}
}

func TestDropLinksMustNameALinkThePredecessorHas(t *testing.T) {
	s := open(t)
	other, _ := s.Add(rec("claim", "Other", nil), agent)
	v1, _ := s.Add(rec("claim", "First wording", nil), agent)

	next := rec("claim", "Second wording", nil)
	next.Supersedes = IDs{v1.ID}
	next.DropLinks = []Link{{Role: "see_also", Record: other.ID}}
	_, err := s.Add(next, agent)
	rejected(t, err, "does not have")

	loose := rec("claim", "Not a supersession", nil)
	loose.DropLinks = []Link{{Role: "see_also", Record: other.ID}}
	_, err = s.Add(loose, agent)
	rejected(t, err, "only applies when superseding")

	if n := count(t, s, `SELECT count(*) FROM records`); n != 2 {
		t.Fatalf("%d records after refused adds, want 2", n)
	}
}

func TestEventRules(t *testing.T) {
	s := open(t)
	c, err := s.Add(rec("claim", "Filing defaults to Opus", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Event(c.ID, "done", "n/a", agent); err == nil || !strings.Contains(err.Error(), "only todos and questions") {
		t.Fatalf("done on a claim: %v", err)
	}
	if err := s.Event(c.ID, "retracted", "wrong", agent); err != nil {
		t.Fatal(err)
	}
	if err := s.Event(c.ID, "retracted", "again", agent); err == nil || !strings.Contains(err.Error(), "retraction is final") {
		t.Fatalf("second retraction: %v", err)
	}
}

func TestADroppedRecordLeavesCurrentAndAsOfReadsButStaysInHistory(t *testing.T) {
	s := open(t)
	pr, err := s.Add(rec("state", "PR 42 open at head abc1234f", map[string]any{"value": "open", "as_of": "2026-10-15", "slot": "pr-heads"}), agent)
	if err != nil {
		t.Fatal(err)
	}
	keep, err := s.Add(rec("claim", "Deploy gate holds the worker change", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	before := time.Now().UTC().Format(time.RFC3339Nano)
	time.Sleep(10 * time.Millisecond)
	if err := s.Event(pr.ID, "dropped", "transient PR state", agent); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.Search(SearchInput{Query: "PR 42"}); len(got) != 0 {
		t.Fatalf("a dropped record is current: %+v", got)
	}
	if d, _ := s.GetAt(pr.ID, AsOf{ValidAt: "2026-10-16"}); d.Status != "dropped" {
		t.Fatalf("as of a day it held, a dropped record reads %s", d.Status)
	}
	if got, _ := s.Search(SearchInput{Query: "PR 42", IncludeHistory: true}); len(got) != 1 || got[0].Status != "dropped" {
		t.Fatalf("history: %+v", got)
	}
	if d, _ := s.GetAt(pr.ID, AsOf{KnownAt: before}); d.Status != "active" {
		t.Fatalf("known before the drop: %s", d.Status)
	}

	next := rec("state", "PR 42 merged", map[string]any{"value": "merged", "as_of": "2026-10-16", "slot": "pr-heads"})
	if _, err := s.Add(next, agent); err != nil {
		t.Fatalf("a dropped state still holds its slot: %v", err)
	}
	next.Supersedes = IDs{pr.ID}
	_, err = s.Add(next, agent)
	rejected(t, err, "dropped")
	_, err = s.Link(keep.ID, []Link{{Role: "see_also", Record: pr.ID}}, agent)
	rejected(t, err, "dropped")
	if err := s.Event(pr.ID, "dropped", "again", agent); err == nil || !strings.Contains(err.Error(), "a drop is final") {
		t.Fatalf("second drop: %v", err)
	}
	if err := s.Event(pr.ID, "retracted", "wrong", agent); err == nil || !strings.Contains(err.Error(), "a drop is final") {
		t.Fatalf("retract after drop: %v", err)
	}
}

func TestAStoreUpgradedToDropsKeepsItsEventGuards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	for _, step := range steps[:len(steps)-1] {
		if _, err := db.Exec(step); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AddEntity(EntityInput{Type: "system", Name: "Agent Supervisor", Sensitivity: "normal", Aliases: []string{"subagents"}}, agent); err != nil {
		t.Fatal(err)
	}
	c, err := s.Add(rec("claim", "Filing defaults to Opus", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Event(c.ID, "retracted", "wrong", agent); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM record_events WHERE event = 'retracted'`); n != 1 {
		t.Fatalf("%d retractions, want 1", n)
	}
	if err := s.Event(c.ID, "dropped", "late", agent); err == nil || !strings.Contains(err.Error(), "retraction is final") {
		t.Fatalf("drop after retraction: %v", err)
	}
}

func TestEntityNamesCannotCollideWithAliases(t *testing.T) {
	s := open(t)
	if _, err := s.AddEntity(EntityInput{Type: "system", Name: "Subagents", Sensitivity: "normal"}, agent); err == nil {
		t.Fatal("an entity named like an existing alias was created")
	}
}

func TestReopenDoesNotReapplySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}
