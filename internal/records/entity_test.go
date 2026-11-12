package records

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

func ids(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func TestSearchCoversAttrsAndSubjectNamesAndMarksPartialMatches(t *testing.T) {
	s := open(t)
	d, err := s.Add(rec("decision", "Filing runs on Haiku", map[string]any{
		"chosen":   "Haiku for every filing call",
		"rejected": []any{map[string]any{"option": "nightly cron batch", "reason": "stale by morning"}},
	}), agent)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Search(SearchInput{Query: "why not the nightly cron"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != d.ID || got[0].Partial {
		t.Fatalf("attrs search: %+v", got)
	}

	got, err = s.Search(SearchInput{Query: "Agent Supervisor"})
	if err != nil || !slices.Contains(ids(got), d.ID) {
		t.Fatalf("subject-name search: %v %+v", err, got)
	}

	got, err = s.Search(SearchInput{Query: "haiku zebra"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Partial {
		t.Fatalf("want one partial match, got %+v", got)
	}

	if _, err := s.EditEntity(EntityEdit{Name: "subagents", Aliases: []string{"workers"}}, "", agent); err != nil {
		t.Fatal(err)
	}
	got, err = s.Search(SearchInput{Query: "workers"})
	if err != nil || len(got) != 1 || got[0].ID != d.ID {
		t.Fatalf("an alias added later is not searchable: %v %+v", err, got)
	}
}

func TestAboutNarrowsToOneKind(t *testing.T) {
	s := open(t)
	if _, err := s.Add(rec("claim", "Filing defaults to Haiku", nil), agent); err != nil {
		t.Fatal(err)
	}
	todo, err := s.Add(rec("todo", "Point filing at Haiku", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.About("subagents", "todo")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Records) != 1 || e.Records[0].ID != todo.ID {
		t.Fatalf("kind filter: %+v", e.Records)
	}
}

func TestMergeFoldsADuplicateIntoTheRemainingEntity(t *testing.T) {
	s := open(t)
	entity(t, s, EntityInput{Type: "system", Name: "Subagent Runner", Sensitivity: "normal", Aliases: []string{"runner"}})
	dupRec := rec("claim", "Runs detached by default", nil)
	dupRec.Subject = "runner"
	own, err := s.Add(dupRec, agent)
	if err != nil {
		t.Fatal(err)
	}
	mentionIn := rec("claim", "Sam reviews every dispatch", nil)
	mentionIn.Subject = "Sam"
	mentionIn.Links = []Link{{Role: "about", Entity: "runner"}}
	mention, err := s.Add(mentionIn, agent)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.EditEntity(EntityEdit{Name: "runner", MergeInto: "subagents"}, "", agent)
	rejected(t, err, "give a reason")
	res, err := s.EditEntity(EntityEdit{Name: "runner", MergeInto: "subagents", Aliases: []string{"bg agents"}}, "same system", agent)
	if err != nil {
		t.Fatal(err)
	}
	if res.MergedFrom == "" || len(res.AddedAliases) != 1 {
		t.Fatalf("result %+v", res)
	}

	for _, name := range []string{"Subagent Runner", "runner", "bg agents"} {
		e, err := s.About(name, "")
		if err != nil {
			t.Fatal(err)
		}
		if e.Name != "Agent Supervisor" {
			t.Fatalf("%s resolved to %s", name, e.Name)
		}
		if !slices.Contains(ids(e.Records), own.ID) || !slices.Contains(ids(e.Mentions), mention.ID) {
			t.Fatalf("%s: records %v mentions %v", name, ids(e.Records), ids(e.Mentions))
		}
		if !slices.Contains(e.Merged, "Subagent Runner") || !slices.Contains(e.Aliases, "runner") {
			t.Fatalf("%s: merged %v aliases %v", name, e.Merged, e.Aliases)
		}
	}

	got, err := s.Search(SearchInput{Subject: "subagents", Query: "detached"})
	if err != nil || len(got) != 1 || got[0].Subject != "Agent Supervisor" {
		t.Fatalf("subject filter after merge: %v %+v", err, got)
	}
	got, err = s.Search(SearchInput{Query: "Subagent Runner"})
	if err != nil || !slices.Contains(ids(got), own.ID) {
		t.Fatalf("merged name not searchable: %v %+v", err, got)
	}

	next := rec("claim", "Runs detached unless asked to wait", nil)
	next.Supersedes = IDs{own.ID}
	if _, err := s.Add(next, agent); err != nil {
		t.Fatalf("superseding a merged entity's record under the remaining one: %v", err)
	}
	if _, err := s.AddEntity(EntityInput{Type: "system", Name: "Subagent Runner", Sensitivity: "normal"}, agent); err == nil {
		t.Fatal("a merged entity's name was reusable")
	}
}

func TestMergeRefusals(t *testing.T) {
	s := open(t)
	for _, e := range []EntityInput{
		{Type: "system", Name: "Runner", Sensitivity: "normal"},
		{Type: "system", Name: "Dispatcher", Sensitivity: "normal"},
		{Type: "person", Name: "Person A", Sensitivity: "phi"},
	} {
		if _, err := s.AddEntity(e, agent); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.EditEntity(EntityEdit{Name: "subagents", MergeInto: "Agent Supervisor"}, "dup", agent)
	rejected(t, err, "already the same entity")
	_, err = s.EditEntity(EntityEdit{Name: "Person A", MergeInto: "Sam"}, "dup", agent)
	rejected(t, err, "different sensitivity")

	if _, err := s.EditEntity(EntityEdit{Name: "Runner", MergeInto: "subagents"}, "dup", agent); err != nil {
		t.Fatal(err)
	}
	_, err = s.EditEntity(EntityEdit{Name: "subagents", MergeInto: "Dispatcher"}, "dup", agent)
	rejected(t, err, "merge the other entity into it instead")
	_, err = s.EditEntity(EntityEdit{Name: "Dispatcher", Aliases: []string{"Runner"}}, "", agent)
	rejected(t, err, "already names entity")
	_, err = s.EditEntity(EntityEdit{Name: "Dispatcher"}, "", agent)
	rejected(t, err, "give aliases")
	if n := count(t, s, `SELECT count(*) FROM entity_merges`); n != 1 {
		t.Fatalf("%d merges written, want 1", n)
	}
}

func TestDescriptionCanBeReplaced(t *testing.T) {
	s := open(t)
	if _, err := s.EditEntity(EntityEdit{Name: "Sam", Description: "Owns the harness"}, "", agent); err != nil {
		t.Fatal(err)
	}
	e, err := s.About("Sam", "")
	if err != nil || e.Description != "Owns the harness" {
		t.Fatalf("description %q, err %v", e.Description, err)
	}
}

func TestUpgradeIndexesRecordsWrittenBeforeIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema + `; ` + step2 + `; PRAGMA user_version = 2;
		INSERT INTO txns (id, actor) VALUES ('tx_1', 'agent');
		INSERT INTO entities (id, type, name, sensitivity, tx_id) VALUES ('ent_1', 'system', 'Filing Pipeline', 'normal', 'tx_1');
		INSERT INTO sources (id, type, ref, tx_id) VALUES ('src_1', 'conversation', 'test', 'tx_1');
		INSERT INTO records (id, kind, subject_id, title, attrs, sensitivity, derived_from, tx_id)
		VALUES ('rec_1', 'claim', 'ent_1', 'Retries three times', '{"workaround":"raise the backoff ceiling"}', 'normal', 'src_1', 'tx_1');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{"backoff ceiling", "filing pipeline"} {
		got, err := s.Search(SearchInput{Query: q})
		if err != nil || len(got) != 1 || got[0].ID != "rec_1" || got[0].Partial {
			t.Fatalf("%q after upgrade: %v %+v", q, err, got)
		}
	}
}
