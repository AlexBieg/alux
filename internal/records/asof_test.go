package records

import (
	"strings"
	"testing"
	"time"
)

func TestAsOfReadsWhatHeldInTheWorldOnADay(t *testing.T) {
	s := open(t)
	window := rec("state", "Canary was at 5%", map[string]any{"value": "5%", "as_of": "2026-03-05"})
	window.ValidFrom, window.ValidUntil = "2026-03-01", "2026-03-10"
	res, err := s.Add(window, agent)
	if err != nil {
		t.Fatal(err)
	}

	if got, _ := s.Search(SearchInput{Kind: "state"}); len(got) != 0 {
		t.Fatalf("an ended state is current today: %+v", got)
	}
	for day, want := range map[string]string{"2026-02-28": "not_yet", "2026-03-01": "active", "2026-03-10": "active", "2026-03-11": "expired"} {
		d, err := s.GetAt(res.ID, AsOf{ValidAt: day})
		if err != nil {
			t.Fatal(err)
		}
		if d.Status != want {
			t.Errorf("as of %s: status %s, want %s", day, d.Status, want)
		}
	}
	e, err := s.AboutAt("subagents", "state", AsOf{ValidAt: "2026-03-05"})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Records) != 1 || e.Records[0].ID != res.ID {
		t.Fatalf("about as of a day inside the window: %+v", e.Records)
	}
	if _, err := s.Search(SearchInput{AsOf: AsOf{ValidAt: "5 March"}}); err == nil || !strings.Contains(err.Error(), "as_of") {
		t.Fatalf("want a malformed as_of refused, got %v", err)
	}
}

func TestKnownAtReadsTheStoreBeforeLaterWrites(t *testing.T) {
	s := open(t)
	old, err := s.Add(rec("claim", "Filing defaults to Opus", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.Add(rec("claim", "Filing runs nightly", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	before := time.Now().UTC().Format(time.RFC3339Nano)
	time.Sleep(10 * time.Millisecond)
	next := rec("claim", "Filing defaults to Haiku", nil)
	next.Supersedes = IDs{old.ID}
	replacement, err := s.Add(next, agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Event(gone.ID, "retracted", "never true", agent); err != nil {
		t.Fatal(err)
	}

	then := AsOf{KnownAt: before}
	got, err := s.Search(SearchInput{Query: "filing", AsOf: then})
	if err != nil {
		t.Fatal(err)
	}
	if ids := ids(got); len(ids) != 2 || !strings.Contains(strings.Join(ids, " "), old.ID) || !strings.Contains(strings.Join(ids, " "), gone.ID) {
		t.Fatalf("known then: %v, want %s and %s", ids, old.ID, gone.ID)
	}
	if d, _ := s.GetAt(old.ID, then); d.Status != "active" {
		t.Fatalf("superseded later, but read as %s before that", d.Status)
	}
	if d, _ := s.Get(old.ID); d.Status != "superseded" {
		t.Fatalf("now: %s", d.Status)
	}
	if _, err := s.GetAt(replacement.ID, then); err == nil || !strings.Contains(err.Error(), "not have been recorded") {
		t.Fatalf("a record written later should be absent then, got %v", err)
	}
}

func TestASlotHoldsOneCurrentStatePerSubject(t *testing.T) {
	s := open(t)
	state := func(title, slot string) RecordInput {
		return rec("state", title, map[string]any{"value": title, "as_of": "2026-10-14", "slot": slot})
	}
	first, err := s.Add(state("PR head aaaa1111", "pr-heads"), agent)
	if err != nil {
		t.Fatal(err)
	}
	r := rejected(t, second(s, state("PR head bbbb2222", "PR-Heads ")), first.ID)
	if !strings.Contains(r.Message, "supersedes") {
		t.Fatalf("refusal should say how to replace the holder: %q", r.Message)
	}
	if _, err := s.Add(state("Canary at 5%", "allocation"), agent); err != nil {
		t.Fatalf("a different slot should coexist: %v", err)
	}
	unrelated, err := s.Add(rec("state", "Unslotted reading", map[string]any{"value": "x", "as_of": "2026-10-14"}), agent)
	if err != nil {
		t.Fatal(err)
	}
	wrong := state("PR head bbbb2222", "pr-heads")
	wrong.Supersedes = IDs{unrelated.ID}
	rejected(t, second(s, wrong), first.ID)

	right := state("PR head bbbb2222", "pr-heads")
	right.Supersedes = IDs{first.ID}
	if _, err := s.Add(right, agent); err != nil {
		t.Fatalf("superseding the holder should take the slot: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM current_records WHERE json_extract(attrs, '$.slot') = 'pr-heads'`); n != 1 {
		t.Fatalf("%d current records fill pr-heads", n)
	}
}

func second(s *Store, in RecordInput) error {
	_, err := s.Add(in, agent)
	return err
}

func TestADecisionCannotBeWrittenAlreadyEnded(t *testing.T) {
	s := open(t)
	ended := rec("decision", "Launch the canary at 1%", map[string]any{"chosen": "1%"})
	ended.ValidUntil = "2026-03-23"
	rejected(t, second(s, ended), "supersede")

	boxed := rec("decision", "Freeze the API until launch", map[string]any{"chosen": "freeze"})
	boxed.ValidUntil = time.Now().AddDate(0, 1, 0).Format("2006-01-02")
	if _, err := s.Add(boxed, agent); err != nil {
		t.Fatalf("a time-boxed decision ending later should be accepted: %v", err)
	}
	reading := rec("state", "Flags read on 2026-08-25", map[string]any{"value": "off", "as_of": "2026-08-25"})
	reading.ValidUntil = "2026-08-26"
	if _, err := s.Add(reading, agent); err != nil {
		t.Fatalf("a historical state may end in the past: %v", err)
	}
}

func TestCitingCountsEachDistinctSourceOnce(t *testing.T) {
	s := open(t)
	claim, err := s.Add(rec("claim", "Graph engines lose on lookup-heavy work", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	other := Source{Type: "url", Ref: "https://example.com/benchmark"}
	for i, want := range []struct {
		src     Source
		added   bool
		sources int
	}{{other, true, 2}, {other, false, 2}, {Source{Type: "conversation", Ref: "test session"}, false, 2}} {
		res, err := s.Cite(claim.ID, want.src, agent)
		if err != nil {
			t.Fatal(err)
		}
		if res.Added != want.added || res.Sources != want.sources {
			t.Errorf("cite %d: %+v, want added=%v sources=%d", i, res, want.added, want.sources)
		}
	}
	got, _ := s.Search(SearchInput{Query: "graph engines"})
	if len(got) != 1 || got[0].Sources != 2 {
		t.Fatalf("search row: %+v", got)
	}
	if d, _ := s.Get(claim.ID); len(d.Cited) != 1 || !strings.Contains(d.Cited[0], other.Ref) {
		t.Fatalf("get lists citations: %v", d.Cited)
	}

	next := rec("claim", "Graph engines lose on one-hop lookup work", nil)
	next.Supersedes = IDs{claim.ID}
	res, err := s.Add(next, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.UncitedSources) != 1 || !strings.Contains(res.UncitedSources[0], other.Ref) {
		t.Fatalf("supersession should report the citation it did not carry: %v", res.UncitedSources)
	}
	if d, _ := s.Get(res.ID); d.Sources != 1 {
		t.Fatalf("new version starts with its own source only, got %d", d.Sources)
	}
	_, err = s.Cite(claim.ID, other, agent)
	rejected(t, err, res.ID)
}
