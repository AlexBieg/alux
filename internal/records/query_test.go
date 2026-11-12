package records

import (
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/taint"
)

func TestSearchRanksCurrentRecordsAndHidesHistoryUnlessAsked(t *testing.T) {
	s := open(t)
	old, err := s.Add(rec("claim", "Filing defaults to Opus", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	next := rec("claim", "Filing defaults to Haiku", nil)
	next.Supersedes = IDs{old.ID}
	if _, err := s.Add(next, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(rec("todo", "Unrelated chore", nil), agent); err != nil {
		t.Fatal(err)
	}

	got, err := s.Search(SearchInput{Query: "what model does filing default to"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "Filing defaults to Haiku" {
		t.Fatalf("current search: %+v", got)
	}
	if got, _ := s.Search(SearchInput{Query: "default"}); len(got) != 1 {
		t.Fatalf("stemmed search returned %d", len(got))
	}
	got, err = s.Search(SearchInput{Query: "filing defaults", IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("history search returned %d", len(got))
	}
	got, err = s.Search(SearchInput{Kind: "todo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "todo" {
		t.Fatalf("kind filter: %+v", got)
	}
}

func TestGetShowsChainLinksAndEvents(t *testing.T) {
	s := open(t)
	q, _ := s.Add(rec("question", "Should filing use Haiku?", nil), agent)
	d := rec("decision", "Filing uses Haiku", map[string]any{"chosen": "haiku"})
	d.Links = []Link{{Role: "answers", Record: q.ID}}
	dec, err := s.Add(d, agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Event(q.ID, "done", "answered", agent); err != nil {
		t.Fatal(err)
	}
	d2 := rec("decision", "Filing uses Haiku by default", map[string]any{"chosen": "haiku default"})
	d2.Supersedes = IDs{dec.ID}
	dec2, err := s.Add(d2, agent)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(dec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "superseded" || !strings.HasPrefix(got.Successor, dec2.ID) || len(got.Links) != 1 {
		t.Fatalf("decision detail: %+v", got)
	}
	if latest, _ := s.Get(dec2.ID); len(latest.Predecessors) != 1 {
		t.Fatalf("predecessors: %v", latest.Predecessors)
	}
	question, _ := s.Get(q.ID)
	if question.Status != "done" || len(question.Events) != 1 || len(question.Backlinks) != 1 {
		t.Fatalf("question detail: %+v", question)
	}
}

func TestLinksFollowTheSupersessionChainInBothDirections(t *testing.T) {
	s := open(t)
	v1, _ := s.Add(rec("claim", "Taint is a convention", nil), agent)
	linker := rec("claim", "Maintenance audits labels", nil)
	linker.Links = []Link{{Role: "supports", Record: v1.ID}}
	current, err := s.Add(linker, agent)
	if err != nil {
		t.Fatal(err)
	}
	staleLinker := rec("claim", "Old wording of a linker", nil)
	staleLinker.Links = []Link{{Role: "see_also", Record: v1.ID}}
	stale, _ := s.Add(staleLinker, agent)
	restated := rec("claim", "New wording of a linker", nil)
	restated.Supersedes = IDs{stale.ID}
	if _, err := s.Add(restated, agent); err != nil {
		t.Fatal(err)
	}
	v2in := rec("claim", "Taint is enforced in the harness", nil)
	v2in.Supersedes = IDs{v1.ID}
	v2, _ := s.Add(v2in, agent)
	v3in := rec("claim", "Taint is enforced by a one-way latch", nil)
	v3in.Supersedes = IDs{v2.ID}
	v3in.Links = []Link{{Role: "see_also", Record: current.ID}}
	v3, err := s.Add(v3in, agent)
	if err != nil {
		t.Fatal(err)
	}
	direct := rec("claim", "Links the newest", nil)
	direct.Links = []Link{{Role: "refines", Record: v3.ID}}
	if _, err := s.Add(direct, agent); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(v3.ID)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got.Backlinks, "\n")
	if !strings.Contains(joined, "supports ← "+current.ID+" Maintenance audits labels (to "+v1.ID+", since replaced)") {
		t.Errorf("a link two versions back is not inherited:\n%s", joined)
	}
	if strings.Contains(joined, stale.ID) {
		t.Errorf("a replaced linker's link is inherited:\n%s", joined)
	}
	if !strings.Contains(joined, "Links the newest") {
		t.Errorf("a direct link is missing:\n%s", joined)
	}

	out, err := s.Get(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "supports → " + v1.ID + " Taint is a convention (now " + v3.ID + " Taint is enforced by a one-way latch)"; len(out.Links) != 1 || out.Links[0] != want {
		t.Errorf("outgoing link = %q, want %q", out.Links, want)
	}
	if latest, _ := s.Get(v3.ID); len(latest.Links) != 1 || strings.Contains(latest.Links[0], "(now") {
		t.Errorf("a link to a current record is annotated: %q", latest.Links)
	}
}

func TestALinkAddedLaterShowsOnBothEndsAndIsDated(t *testing.T) {
	s := open(t)
	anchor, _ := s.Add(rec("decision", "Filing uses Haiku", map[string]any{"chosen": "haiku"}), agent)
	claim, _ := s.Add(rec("claim", "Haiku is cheap", nil), agent)
	res, err := s.Link(claim.ID, []Link{{Role: "supports", Record: anchor.ID}, {Role: "about", Entity: "Sam"}}, agent)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 {
		t.Fatalf("added %d, want 2", res.Added)
	}
	again, err := s.Link(claim.ID, []Link{{Role: "supports", Record: anchor.ID}}, agent)
	if err != nil || again.Added != 0 || again.AlreadyLinked != 1 {
		t.Fatalf("repeat link: %+v, %v", again, err)
	}
	got, _ := s.Get(claim.ID)
	if len(got.Links) != 2 || !strings.Contains(got.Links[0], "supports → "+anchor.ID) || !strings.Contains(got.Links[0], "(linked ") {
		t.Errorf("links = %q", got.Links)
	}
	if back, _ := s.Get(anchor.ID); len(back.Backlinks) != 1 || !strings.Contains(back.Backlinks[0], claim.ID) {
		t.Errorf("backlinks = %q", back.Backlinks)
	}
	written := rec("claim", "Linked at write", nil)
	written.Links = []Link{{Role: "see_also", Record: anchor.ID}}
	w, _ := s.Add(written, agent)
	if got, _ := s.Get(w.ID); len(got.Links) != 1 || strings.Contains(got.Links[0], "(linked ") {
		t.Errorf("a link written with the record is dated: %q", got.Links)
	}
	if n := count(t, s, `SELECT count(*) FROM records`); n != 3 {
		t.Errorf("linking wrote records: %d", n)
	}
}

func TestLinkRefusesReplacedRetractedAndInvalidLinks(t *testing.T) {
	s := open(t)
	v1, _ := s.Add(rec("claim", "First wording", nil), agent)
	next := rec("claim", "Second wording", nil)
	next.Supersedes = IDs{v1.ID}
	v2, _ := s.Add(next, agent)
	gone, _ := s.Add(rec("claim", "Wrong", nil), agent)
	if err := s.Event(gone.ID, "retracted", "wrong", agent); err != nil {
		t.Fatal(err)
	}
	other, _ := s.Add(rec("claim", "Other", nil), agent)

	_, err := s.Link(other.ID, []Link{{Role: "see_also", Record: v1.ID}}, agent)
	rejected(t, err, "replaced by "+v2.ID)
	_, err = s.Link(v1.ID, []Link{{Role: "see_also", Record: other.ID}}, agent)
	rejected(t, err, "replaced by "+v2.ID)
	_, err = s.Link(other.ID, []Link{{Role: "see_also", Record: gone.ID}}, agent)
	rejected(t, err, "retracted")
	_, err = s.Link(other.ID, []Link{{Role: "see_also", Record: other.ID}}, agent)
	rejected(t, err, "")
	_, err = s.Link(other.ID, []Link{{Role: "see_also", Record: v2.ID}, {Role: "see_also", Entity: "nobody"}}, agent)
	rejected(t, err, "unknown entity")
	if n := count(t, s, `SELECT count(*) FROM links WHERE from_record = ?`, other.ID); n != 0 {
		t.Errorf("a refused link call wrote %d links", n)
	}
}

func TestAnInheritedLinkIsNotShownTwiceWhenTheLinkerAlsoLinksTheNewVersion(t *testing.T) {
	s := open(t)
	v1, _ := s.Add(rec("claim", "First wording", nil), agent)
	next := rec("claim", "Second wording", nil)
	next.Supersedes = IDs{v1.ID}
	v2, _ := s.Add(next, agent)
	linker := rec("claim", "Linker", nil)
	linker.Links = []Link{{Role: "supports", Record: v1.ID}, {Role: "supports", Record: v2.ID}}
	if _, err := s.Add(linker, agent); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Backlinks) != 1 || strings.Contains(got.Backlinks[0], "since replaced") {
		t.Errorf("backlinks = %q, want the one direct link", got.Backlinks)
	}
}

func TestAboutListsCurrentRecordsAndAliases(t *testing.T) {
	s := open(t)
	if _, err := s.Add(rec("claim", "Filing defaults to Haiku", nil), agent); err != nil {
		t.Fatal(err)
	}
	e, err := s.About("subagents", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "Agent Supervisor" || len(e.Aliases) != 1 || len(e.Records) != 1 {
		t.Fatalf("about: %+v", e)
	}
}

func TestReadingPHITaints(t *testing.T) {
	s := open(t)
	if _, err := s.AddEntity(EntityInput{Type: "person", Name: "Person A", Sensitivity: "phi"}, agent); err != nil {
		t.Fatal(err)
	}
	r := rec("claim", "Some detail", nil)
	r.Subject, r.Sensitivity = "Person A", "phi"
	if _, err := s.Add(r, agent); err != nil {
		t.Fatal(err)
	}

	taint.ResetForTest()
	t.Cleanup(taint.ResetForTest)
	if _, err := s.Search(SearchInput{Query: "filing"}); err != nil || taint.Tainted() {
		t.Fatalf("a search returning no phi tainted (err %v)", err)
	}
	if _, err := s.Search(SearchInput{Query: "detail"}); err != nil || !taint.Tainted() {
		t.Fatalf("a search returning phi did not taint (err %v)", err)
	}
	taint.ResetForTest()
	if _, err := s.About("Person A", ""); err != nil || !taint.Tainted() {
		t.Fatalf("reading a phi entity did not taint (err %v)", err)
	}
}

func TestOpenUnfiledHidesResolved(t *testing.T) {
	s := open(t)
	if _, err := s.Unfile("no_kind_fits", "", []byte(`{"text":"a risk"}`), "a risk", "normal", agent); err != nil {
		t.Fatal(err)
	}
	id, err := s.Unfile("other", "", []byte(`{"text":"a merge"}`), "a merge", "normal", agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveUnfiled(id, "schema_changed", "", "entity_edit can merge now", agent); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenUnfiled(0)
	if err != nil || len(got) != 1 || got[0].Summary != "a risk" {
		t.Fatalf("open unfiled: %+v %v", got, err)
	}
}

func TestResolveUnfiledRefusesWhatCannotBeRecorded(t *testing.T) {
	s := open(t)
	id, err := s.Unfile("no_kind_fits", "", []byte(`{}`), "a thing", "normal", agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id                    int64
		outcome, record, note string
		want                  string
	}{
		{id, "filed", "", "", "needs the id of the record"},
		{id, "filed", "rec_missing", "", "no record"},
		{id, "discarded", "", "", "needs a reason"},
		{id, "ignored", "", "x", "unknown outcome"},
		{id + 1, "discarded", "", "x", "no unfiled entry"},
	} {
		if err := s.ResolveUnfiled(c.id, c.outcome, c.record, c.note, agent); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: got %v, want %q", c, err, c.want)
		}
	}
	if err := s.ResolveUnfiled(id, "discarded", "", "duplicate", agent); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveUnfiled(id, "discarded", "", "again", agent); err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("second resolution: %v", err)
	}
}
