package records

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// conceptEmbedder puts each word on the axis of the concept it belongs to, so synonyms land together.
type conceptEmbedder struct{ down bool }

var concepts = map[string]int{"ripple": 0, "replay": 0, "nap": 0, "sleep": 0, "forecast": 1, "budget": 1, "grinder": 2, "burr": 2}

func (c *conceptEmbedder) Model() string { return "concepts" }

func (c *conceptEmbedder) vec(s string) []float32 {
	v := make([]float32, 4)
	v[3] = 0.01
	for _, w := range strings.Fields(strings.ToLower(s)) {
		if d, ok := concepts[w]; ok {
			v[d]++
		}
	}
	normalize(v)
	return v
}

func (c *conceptEmbedder) Query(_ context.Context, q string) ([]float32, error) {
	if c.down {
		return nil, errors.New("not answering")
	}
	return c.vec(q), nil
}

func (c *conceptEmbedder) Documents(_ context.Context, titles, texts []string) ([][]float32, error) {
	if c.down {
		return nil, errors.New("not answering")
	}
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = c.vec(titles[i] + " " + texts[i])
	}
	return out, nil
}

func TestSimilarRecordsLogCosineWithoutChangingWhatIsShownOrHowItRanks(t *testing.T) {
	draft := from(rec("claim", "Ripple replay during sleep consolidates memories", nil), "b.md")
	hold := func(emb Embedder) (*Store, Result) {
		t.Helper()
		s := open(t)
		if emb != nil {
			if err := s.SetEmbedder(emb); err != nil {
				t.Fatal(err)
			}
		}
		write(t, s, from(rec("claim", "Ripple replay happens in slow-wave sleep", nil), "a.md"))
		write(t, s, from(rec("claim", "The grinder consolidates beans before the burr", nil), "a.md"))
		return s, write(t, s, draft)
	}
	_, plainRes := hold(nil)
	s, res := hold(&conceptEmbedder{})
	plain, cands := plainRes.Similar, res.Similar
	if len(cands) != 2 || len(plain) != 2 {
		t.Fatalf("want both records shown as similar, got %+v and %+v", plain, cands)
	}
	for i := range cands {
		if cands[i].Title != plain[i].Title || cands[i].Score != plain[i].Score {
			t.Fatalf("cosine changed the ranking: %+v against %+v", cands, plain)
		}
	}
	cosine := map[string]float64{}
	for _, c := range cands {
		cosine[c.Title] = c.features["cosine"]
		if !slices.ContainsFunc(c.Signals, func(sig string) bool { return strings.HasPrefix(sig, "cosine ") }) {
			t.Errorf("no cosine signal on %+v", c)
		}
	}
	if near, far := cosine["Ripple replay happens in slow-wave sleep"], cosine["The grinder consolidates beans before the burr"]; near < 0.9 || far > 0.1 {
		t.Errorf("cosine near %v far %v", near, far)
	}

	if n := count(t, s, `SELECT count(*) FROM candidate_suggestions WHERE written_id = ? AND json_extract(features, '$.cosine') IS NOT NULL`, res.ID); n != 2 {
		t.Errorf("%d suggestions logged a cosine, want 2", n)
	}
	if n := count(t, s, `SELECT count(*) FROM record_embeddings WHERE record_id = ?`, res.ID); n != 1 {
		t.Errorf("the written record has %d vectors before any search, want 1", n)
	}

	_, down := hold(&conceptEmbedder{down: true})
	for _, c := range down.Similar {
		if _, ok := c.features["cosine"]; ok {
			t.Errorf("with the embedder down a cosine was logged: %+v", c)
		}
	}
}

func TestSearchFindsRecordsByMeaningAndFollowsTheirLinks(t *testing.T) {
	s := open(t)
	emb := &conceptEmbedder{}
	if err := s.SetEmbedder(emb); err != nil {
		t.Fatal(err)
	}
	entity(t, s, EntityInput{Type: "topic", Name: "Budget", Sensitivity: "normal"})
	ripple := write(t, s, rec("claim", "Ripple replay consolidates memories", nil))
	forecast := rec("decision", "Quarterly forecast moves to Fridays", map[string]any{"chosen": "Fridays"})
	forecast.Subject, forecast.Links = "Budget", []Link{{Role: "depends_on", Record: ripple.ID}}
	linked := write(t, s, forecast)
	write(t, s, rec("claim", "The grinder needs a new burr", nil))

	got, note, err := s.SearchNote(SearchInput{Query: "afternoon nap"})
	if err != nil || note != "" {
		t.Fatalf("%v %q", err, note)
	}
	if len(got) != 2 || got[0].ID != ripple.ID || !got[0].Similar || got[1].ID != linked.ID || !got[1].Linked {
		t.Fatalf("want the record close in meaning, then the record linked to it: %+v", got)
	}

	if got, note, err := s.SearchNote(SearchInput{Query: "qwzx blorptang"}); err != nil || note != "" || len(got) != 0 {
		t.Fatalf("a query close in meaning to nothing finds nothing: %+v %q %v", got, note, err)
	}

	emb.down = true
	got, note, err = s.SearchNote(SearchInput{Query: "grinder"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "The grinder needs a new burr" || !strings.Contains(note, "unavailable") {
		t.Fatalf("with the embedder down, keyword matches still come back with a note: %+v %q", got, note)
	}
	if got, note, _ := s.SearchNote(SearchInput{Query: "afternoon nap"}); len(got) != 0 || note == "" {
		t.Fatalf("a query with no keyword match and no embedder finds nothing and says why: %+v %q", got, note)
	}
}
