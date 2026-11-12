package ui

import (
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/awake"
)

func TestTheStatusLineSaysWhenAluxKeepsTheMacAwake(t *testing.T) {
	m, _ := harness(t, 60, 24)
	settle(m)
	for state, want := range map[awake.State]string{
		awake.Off:        "",
		awake.Holding:    awakeMark,
		awake.LowBattery: "battery under 20%, letting the Mac sleep",
	} {
		m.snap.Awake = state
		status := edgeRow(t, m)
		if want == "" {
			if strings.Contains(status, awakeMark) || strings.Contains(status, "sleep") {
				t.Errorf("status %q mentions sleep with nothing in flight", status)
			}
			continue
		}
		if !strings.Contains(status, want) {
			t.Errorf("status %q on a 60-column pane, want it to keep %q", status, want)
		}
	}
}
