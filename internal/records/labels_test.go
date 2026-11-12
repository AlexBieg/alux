package records

import (
	"testing"

	"github.com/alexbieg/alux/internal/taint"
)

func TestLabelNamesIDsAndSkipsPHIWithoutTainting(t *testing.T) {
	taint.ResetForTest()
	t.Cleanup(taint.ResetForTest)
	s := open(t)
	sam := entityID(t, s, "Sam")
	patient, err := s.AddEntity(EntityInput{Type: "person", Name: "Jane Person", Sensitivity: "phi"}, agent)
	if err != nil {
		t.Fatal(err)
	}
	normal, err := s.Add(rec("claim", "Runs detached by default", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	secret := rec("claim", "Diagnosed with something", nil)
	secret.Sensitivity = "phi"
	secret.Source = Source{Type: "conversation", Ref: "phi session"}
	phiRecord, err := s.Add(secret, agent)
	if err != nil {
		t.Fatal(err)
	}
	var phiSource string
	if err := s.db.QueryRow(`SELECT derived_from FROM records WHERE id = ?`, phiRecord.ID).Scan(&phiSource); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		id, want string
		ok       bool
	}{
		{sam, "Sam", true},
		{normal.ID, "Agent Supervisor: Runs detached by default", true},
		{patient.ID, "", false},
		{phiRecord.ID, "", false},
		{phiSource, "", false},
		{"ent_0000000000000000", "", false},
	} {
		got, ok, err := s.Label(c.id)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want || ok != c.ok {
			t.Errorf("Label(%s) = %q, %v; want %q, %v", c.id, got, ok, c.want, c.ok)
		}
	}
	if taint.Tainted() {
		t.Fatal("labelling phi ids set the taint latch")
	}
}

func TestTicketLabelReadsTheEntityNamedByKeyOrAlias(t *testing.T) {
	taint.ResetForTest()
	t.Cleanup(taint.ResetForTest)
	s := open(t)
	for _, in := range []EntityInput{
		{Type: "ticket", Name: "ABC-101", Description: "Make export rows optional", Sensitivity: "normal"},
		{Type: "ticket", Name: "Invoice export", Sensitivity: "normal", Aliases: []string{"ABC-12"}},
		{Type: "ticket", Name: "ABC-99", Description: "Patient name here", Sensitivity: "phi"},
	} {
		entity(t, s, in)
	}
	for key, want := range map[string]string{"ABC-101": "Make export rows optional", "ABC-12": "Invoice export", "ABC-99": "", "ABC-1": ""} {
		got, ok, err := s.TicketLabel(key)
		if err != nil {
			t.Fatal(err)
		}
		if got != want || ok != (want != "") {
			t.Errorf("TicketLabel(%s) = %q, %v; want %q", key, got, ok, want)
		}
	}
	if taint.Tainted() {
		t.Fatal("labelling a phi ticket set the taint latch")
	}
}

func entityID(t *testing.T, s *Store, name string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM entities WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
