package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/charmbracelet/x/ansi"
)

// passAll stands in for guardian in tests about something else.
type passAll struct{}

func (passAll) Screen(context.Context, string) guardian.Verdict {
	return guardian.Verdict{Agent: "test", Top: guardian.Read, Threshold: 0.5}
}
func (passAll) Record(string, guardian.Verdict, string) {}
func (passAll) Suggest(context.Context, string, []string) guardian.Suggestion {
	return guardian.Suggestion{}
}
func (passAll) RecordSuggested(string, guardian.Verdict, guardian.Suggestion, string, string) {}

func TestGuardiansVerdictShowsOnTheHeadAfterTheBoxIsDrawn(t *testing.T) {
	m, s := harness(t, 150, 24)
	s.StartToolForTest("t1", "$ git push", agent.KindBash)
	s.ResolveToolForTest("t1", "$ git push", numberedLines(11), agent.KindBash)
	settle(m)
	if row := ansi.Strip(rowWith(t, m, "git push")); strings.Contains(row, "guardian") {
		t.Fatalf("a verdict showed before one landed: %q", row)
	}

	s.GuardianForTest("t1", guardian.Verdict{Top: guardian.Network, Danger: 0.91, Flagged: true})
	settle(m)
	row := ansi.Strip(rowWith(t, m, "git push"))
	if !strings.Contains(row, "guardian ✗ uses the network · high risk · you allowed it") || !strings.Contains(row, "11 lines") {
		t.Errorf("head does not carry the verdict beside the line count: %q", row)
	}
}

func TestAPassingVerdictIsMarkedSo(t *testing.T) {
	m, s := harness(t, 100, 24)
	s.StartToolForTest("t1", "$ jj status", agent.KindBash)
	s.ResolveToolForTest("t1", "$ jj status", "clean", agent.KindBash)
	s.GuardianForTest("t1", guardian.Verdict{Top: guardian.Read, Danger: 0.03})
	settle(m)
	if row := ansi.Strip(rowWith(t, m, "jj status")); !strings.Contains(row, "guardian ✓ reads files · low risk") {
		t.Errorf("passing verdict not shown: %q", row)
	}
}

func TestAFlaggedVerdictUnderThreeQuartersIsMediumRisk(t *testing.T) {
	m, s := harness(t, 150, 24)
	s.StartToolForTest("t1", "$ jj op abandon", agent.KindBash)
	s.ResolveToolForTest("t1", "$ jj op abandon", "done", agent.KindBash)
	s.GuardianForTest("t1", guardian.Verdict{Top: guardian.Record, Danger: 0.4, Flagged: true})
	settle(m)
	if row := ansi.Strip(rowWith(t, m, "jj op abandon")); !strings.Contains(row, "records in version control · medium risk") {
		t.Errorf("flagged verdict under 0.75 not medium: %q", row)
	}
}

func TestACommandRunOutsideTheSandboxSaysSoOnItsHead(t *testing.T) {
	if w := ansi.StringWidth(unsandboxedMark); w != 1 {
		t.Fatalf("the mark counts %d cells, but the lead counts it as one", w)
	}
	m, s := harness(t, 100, 24)
	s.StartToolForTest("t1", "$ git push", agent.KindBash)
	s.StartToolForTest("t2", "$ jj status", agent.KindBash)
	s.UnsandboxedForTest("t1")
	s.ResolveToolForTest("t1", "$ git push", "pushed", agent.KindBash)
	s.ResolveToolForTest("t2", "$ jj status", "clean", agent.KindBash)
	settle(m)
	if row := ansi.Strip(rowWith(t, m, "git push")); !strings.Contains(row, unsandboxedMark+" $ git push") {
		t.Errorf("an unsandboxed command's head does not say so: %q", row)
	}
	if row := ansi.Strip(rowWith(t, m, "jj status")); strings.Contains(row, unsandboxedMark) {
		t.Errorf("a sandboxed command's head was marked: %q", row)
	}
}
