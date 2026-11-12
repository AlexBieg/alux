package records

import (
	"slices"
	"testing"
)

func TestSearchFollowsLinksFromItsMatchesToRecordsSharingNoWords(t *testing.T) {
	s := open(t)
	for _, name := range []string{"Sleep", "Budget"} {
		entity(t, s, EntityInput{Type: "topic", Name: name, Sensitivity: "normal"})
	}
	on := func(subject, kind, title string, attrs map[string]any, links ...Link) RecordInput {
		in := rec(kind, title, attrs)
		in.Subject, in.Links = subject, links
		return in
	}
	ripple := write(t, s, on("Sleep", "claim", "Ripple replay consolidates memories", nil))
	forecast := write(t, s, on("Budget", "decision", "Quarterly forecast moves to Fridays", map[string]any{"chosen": "Fridays"},
		Link{Role: "depends_on", Record: ripple.ID}))
	sibling := write(t, s, on("Sleep", "claim", "The grinder needs a new burr", nil))

	got, err := s.Search(SearchInput{Query: "ripple replay"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != ripple.ID || got[0].Linked || got[1].ID != forecast.ID || !got[1].Linked {
		t.Fatalf("want the match then the record linked to it: %+v", got)
	}
	if slices.Contains(ids(got), sibling.ID) {
		t.Fatalf("a record sharing only the subject was returned: %+v", got)
	}

	in := on("Sleep", "claim", "Ripple replay consolidates memories offline", nil)
	in.Supersedes = IDs{ripple.ID}
	newer := write(t, s, in)
	got, err = s.Search(SearchInput{Query: "ripple replay"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got), []string{newer.ID, forecast.ID}) {
		t.Fatalf("a link to a superseded record should reach through to its successor: %+v", got)
	}

	for name, in := range map[string]SearchInput{
		"kind filter":     {Query: "ripple replay", Kind: "claim"},
		"subject filter":  {Query: "ripple replay", Subject: "Sleep"},
		"include_history": {Query: "ripple replay", IncludeHistory: true},
		"as_of":           {Query: "ripple replay", AsOf: AsOf{ValidAt: "2099-01-01"}},
	} {
		got, err := s.Search(in)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(ids(got), forecast.ID) {
			t.Fatalf("%s: the linked record should not be added: %+v", name, got)
		}
	}
}
