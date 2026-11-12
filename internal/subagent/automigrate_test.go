package subagent

import (
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
)

func TestAStartedMigrationCarriesItsClaimAndNeverReachesTheMainAgent(t *testing.T) {
	def := echo(true)
	def.Name = migratorName
	def.Commands = lookOnly(migratorName)
	m, _ := manager(t, def, mockapi.Turn{Text: "Migrated Systems/A.md into rec_1."})
	var notices []string
	m.Notify = func(s string) { notices = append(notices, s) }
	announced := 0

	run, err := m.StartMigration(agent.Callbacks{OnRun: func(int) { announced++ }}, "Systems/A.md", "clm_abc")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("migration never finished")
	}
	got, _ := m.Roster.Find(run)
	if !strings.Contains(got.Task, "Systems/A.md") || !strings.Contains(got.Task, "clm_abc") || got.Name != "migrate A" {
		t.Fatalf("run should name the note and carry its claim: %+v", got)
	}
	if announced != 1 || len(notices) != 1 {
		t.Fatalf("want one box and one notice, got %d boxes and notices %v", announced, notices)
	}
	if out := m.Roster.TakeOutcomes(); len(out) != 0 {
		t.Fatalf("an automatic migration was queued for the main agent: %+v", out)
	}
}

func TestStartMigrationRefusesWithoutAMigrator(t *testing.T) {
	m, _ := manager(t, echo(true))
	if m.Has(migratorName) {
		t.Fatal("Has reports a migrator that was never registered")
	}
	if _, err := m.StartMigration(agent.Callbacks{}, "A.md", "clm_x"); err == nil {
		t.Fatal("want an error with no migrator registered")
	}
}
