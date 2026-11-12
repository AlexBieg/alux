package subagent

import (
	"testing"

	"github.com/alexbieg/alux/internal/report"
)

func TestARunningRunsStateReadsAsActive(t *testing.T) {
	cell := ReportTable([]Run{{Agent: "research", State: Running, Step: 3, MaxSteps: 40}}).Rows[0].Cells[2]
	if cell.Tone != report.Active {
		t.Fatalf("state %q has tone %q, want active", cell.Text, cell.Tone)
	}
}
