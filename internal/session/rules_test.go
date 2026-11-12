package session

import (
	"testing"

	"github.com/alexbieg/alux/internal/agent"
)

func TestFiredRulesShowAsACollapsedBoxOfCategoriesAndCountListingEachRule(t *testing.T) {
	s, _ := newTestSession(t)
	s.pushRules([]agent.FiredRule{
		{Subject: "Code Comments", Title: "Default to no comment"},
		{Subject: "Code preferences", Title: "Run the pass before done"},
		{Subject: "Code Comments", Title: "Never restate the next line"},
	})
	items := s.Snapshot().Items
	box, ok := Collapsible(items[len(items)-1], nil, nil)
	if !ok {
		t.Fatal("the fired rules are not a box")
	}
	if box.Head != "3 rules · Code Comments, Code preferences" {
		t.Errorf("head = %q, want the count and each category once", box.Head)
	}
	want := "- Code Comments: Default to no comment\n- Code preferences: Run the pass before done\n- Code Comments: Never restate the next line"
	if box.Body != want {
		t.Errorf("body = %q, want one list item per rule", box.Body)
	}
	if box.OpenByDefault() {
		t.Error("a short rules box starts open; it should start collapsed")
	}
}
