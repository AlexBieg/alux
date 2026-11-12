package subagent

import (
	"slices"
	"testing"

	"github.com/alexbieg/alux/internal/records"
)

func TestEveryShippedSubagentCanBeNamedByATriggerAgentKey(t *testing.T) {
	root := t.TempDir()
	names := []string{ReviewerName, migratorName, curatorName, WalkthroughName}
	for _, def := range []*Definition{Research(), coderDef(root, nil), pairedCoderDef(root, nil), Verifier(),
		Navigator(), Planner(root)} {
		names = append(names, def.Name)
	}
	for _, name := range names {
		if !slices.Contains(records.TriggerAgents, name) {
			t.Errorf("subagent %q is missing from records.TriggerAgents", name)
		}
	}
	for _, name := range records.TriggerAgents {
		if !slices.Contains(names, name) {
			t.Errorf("records.TriggerAgents names %q, which no shipped subagent is called", name)
		}
	}
}
