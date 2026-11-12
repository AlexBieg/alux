package records

import (
	"testing"
)

func TestLookAlikePairsLeaveTheQueueOnceLinkedOrJudgedAndReturnWhenRewritten(t *testing.T) {
	s := open(t)
	if err := s.SetEmbedder(&conceptEmbedder{}); err != nil {
		t.Fatal(err)
	}
	a := write(t, s, rec("claim", "Ripple replay consolidates memories", nil))
	b := write(t, s, rec("claim", "Sleep replay during a nap", nil))
	write(t, s, rec("claim", "The grinder needs a new burr", nil))
	phi := rec("claim", "Ripple sleep replay seen in a patient", nil)
	phi.Sensitivity = "phi"
	write(t, s, phi)

	queue := func() []Pair {
		t.Helper()
		pairs, total, err := s.Pairs(50)
		if err != nil {
			t.Fatal(err)
		}
		n, err := s.PairBacklog()
		if err != nil || n != total || len(pairs) != total {
			t.Fatalf("backlog %d (%v), total %d, listed %d", n, err, total, len(pairs))
		}
		return pairs
	}
	only := func(x, y string) {
		t.Helper()
		got := queue()
		if len(got) != 1 {
			t.Fatalf("want one pair %s %s, got %+v", x, y, got)
		}
		p := got[0]
		if !(p.A.ID == x && p.B.ID == y || p.A.ID == y && p.B.ID == x) || p.Cosine < PairFloor {
			t.Fatalf("want %s and %s, got %s and %s at %.3f", x, y, p.A.ID, p.B.ID, p.Cosine)
		}
	}

	only(a.ID, b.ID)

	if err := s.Judge(b.ID, a.ID, "distinct", "only look alike", agent); err != nil {
		t.Fatal(err)
	}
	if got := queue(); len(got) != 0 {
		t.Fatalf("a judged pair stays out of the queue: %+v", got)
	}
	if n := count(t, s, `SELECT count(*) FROM pair_verdicts WHERE verdict = 'distinct' AND cosine > 0.9 AND model = 'concepts'`); n != 1 {
		t.Fatalf("the verdict keeps the cosine and model it was judged at: %d rows", n)
	}

	if err := s.Judge(a.ID, b.ID, "reopened", "", agent); err != nil {
		t.Fatal(err)
	}
	only(a.ID, b.ID)

	if _, err := s.Link(a.ID, []Link{{Role: "see_also", Record: b.ID}}, agent); err != nil {
		t.Fatal(err)
	}
	if got := queue(); len(got) != 0 {
		t.Fatalf("a linked pair is not queued: %+v", got)
	}

	rewrite := rec("claim", "Sleep replay during a short nap", nil)
	rewrite.Supersedes = IDs{b.ID}
	b2 := write(t, s, rewrite)
	if got := queue(); len(got) != 0 {
		t.Fatalf("a link to the old version still joins the new one: %+v", got)
	}

	c := write(t, s, rec("claim", "Replay in sleep", nil))
	if err := s.Judge(a.ID, c.ID, "distinct", "", agent); err != nil {
		t.Fatal(err)
	}
	if err := s.Judge(b2.ID, c.ID, "related", "", agent); err != nil {
		t.Fatal(err)
	}
	if got := queue(); len(got) != 0 {
		t.Fatalf("want an empty queue, got %+v", got)
	}
	again := rec("claim", "Replay in deep sleep", nil)
	again.Supersedes = IDs{c.ID}
	c2 := write(t, s, again)
	got := queue()
	if len(got) != 2 {
		t.Fatalf("a rewritten record's verdicts do not carry, so both its pairs return: %+v", got)
	}
	for _, p := range got {
		if p.A.ID != c2.ID && p.B.ID != c2.ID {
			t.Fatalf("only the rewritten record's pairs return: %+v", p)
		}
	}
}

func TestJudgeRefusesWhatItCannotStand(t *testing.T) {
	s := open(t)
	if err := s.SetEmbedder(&conceptEmbedder{}); err != nil {
		t.Fatal(err)
	}
	a := write(t, s, rec("claim", "Ripple replay consolidates memories", nil))
	b := write(t, s, rec("claim", "Sleep replay during a nap", nil))
	phi := rec("claim", "Ripple sleep replay seen in a patient", nil)
	phi.Sensitivity = "phi"
	p := write(t, s, phi)
	newer := rec("claim", "Sleep replay during a short nap", nil)
	newer.Supersedes = IDs{b.ID}
	b2 := write(t, s, newer)

	rejected(t, s.Judge(a.ID, a.ID, "distinct", "", agent), "same id")
	rejected(t, s.Judge(a.ID, b2.ID, "maybe", "", agent), "verdict must be")
	rejected(t, s.Judge(a.ID, b.ID, "distinct", "", agent), "replaced by "+b2.ID)
	rejected(t, s.Judge(a.ID, p.ID, "distinct", "", agent), "phi")
	if n := count(t, s, `SELECT count(*) FROM pair_verdicts`); n != 0 {
		t.Fatalf("a refused verdict wrote %d rows", n)
	}
}

func TestPairsNeedAnEmbedderButTheBacklogDoesNot(t *testing.T) {
	s := open(t)
	write(t, s, rec("claim", "Ripple replay consolidates memories", nil))
	if _, _, err := s.Pairs(10); err != ErrNoEmbedder {
		t.Fatalf("want ErrNoEmbedder, got %v", err)
	}
	if n, err := s.PairBacklog(); err != nil || n != 0 {
		t.Fatalf("backlog %d %v, want 0 and no error", n, err)
	}
}
