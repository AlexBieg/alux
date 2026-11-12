package records

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAGistIsWrittenOnceAndOnlyForASessionThatWroteRecords(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in := Actor{Kind: "agent", Agent: "main", Session: "s1"}
	if _, err := s.AddEntity(EntityInput{Type: "system", Name: "alux", Sensitivity: "normal"}, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(RecordInput{Kind: "claim", Subject: "alux", Title: "A fact", Sensitivity: "normal",
		Source: Source{Type: "conversation", Ref: "t"}}, in); err != nil {
		t.Fatal(err)
	}
	gist := Gist{Goal: "g", Outcome: "o"}
	curator := Actor{Kind: "agent", Agent: "curator"}

	if err := s.WriteEpisode(EpisodeInput{Session: "none", Title: "Nothing written", Gist: gist, Sensitivity: "normal"}, curator); err == nil ||
		!strings.Contains(err.Error(), "wrote no records") {
		t.Errorf("a session with no records was gisted: %v", err)
	}
	if left, _ := s.UngistedSessions(); len(left) != 1 || left[0] != "s1" {
		t.Errorf("ungisted before = %v, want [s1]", left)
	}
	if err := s.WriteEpisode(EpisodeInput{Session: "s1", Title: "First gist", Gist: gist, Sensitivity: "normal"}, curator); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteEpisode(EpisodeInput{Session: "s1", Title: "Second gist", Gist: gist, Sensitivity: "normal"}, curator); err == nil {
		t.Error("a session was gisted twice")
	}
	if left, _ := s.UngistedSessions(); len(left) != 0 {
		t.Errorf("ungisted after = %v, want none", left)
	}
}
