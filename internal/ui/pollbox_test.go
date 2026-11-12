package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/alexbieg/alux/internal/agent"
)

func openBox(t *testing.T, m *Model, head string) {
	t.Helper()
	if row := rowContaining(t, m, head); strings.Contains(strings.Split(screen(m), "\n")[row], "▸") {
		expand(t, m, row)
	}
}

func TestAPollBoxLeadsWithItsCommandAndSaysHowItEnded(t *testing.T) {
	m, s := harness(t, 100, 30)
	head := "poll every 10s until exit 0, gives up after 30m: curl -fsS localhost:3000/health · check-in 30m"
	s.StartToolForTest("p1", head, agent.KindPoll)
	s.ResolveToolForTest("p1", head, "11:40:01 build\tpending\nlint\tpass\n11:40:41 ok\n[condition met after 40s, 5 checks]", agent.KindPoll)
	settle(m)
	openBox(t, m, "curl -fsS localhost:3000/health")

	lines := strings.Split(screen(m), "\n")
	at := rowContaining(t, m, "curl -fsS localhost:3000/health")
	if strings.Contains(lines[at], "poll every") {
		t.Errorf("the command is still buried in the settings: %q", lines[at])
	}
	between := strings.Join(lines[at+1:rowContaining(t, m, "11:40:01")], "\n")
	settings, verdict := strings.Index(between, "until exit 0 · every 10s · up to 30m"), strings.Index(between, "✓ condition met after 40s")
	if settings < 0 || verdict < settings {
		t.Errorf("between the command and the log, want the settings then the verdict:\n%s", between)
	}
	if strings.Contains(screen(m), "[condition met") {
		t.Error("the closing [status] line is drawn as log output as well as the verdict")
	}
	build, lint := lines[rowContaining(t, m, "build")], lines[rowContaining(t, m, "lint")]
	if col := ansi.StringWidth(build[:strings.Index(build, "build")]); col != ansi.StringWidth(lint[:strings.Index(lint, "lint")]) {
		t.Errorf("a check's second line is not hung under its first:\n%s\n%s", build, lint)
	}
}

func TestANewPollBoxShowsItsFirstCheckNotTheToolResultProse(t *testing.T) {
	m, s := harness(t, 100, 30)
	head := "poll every 30s until /deployed/, gives up after 30m: kubectl rollout status deploy/web · check-in 30m"
	s.StartToolForTest("p1", head, agent.KindPoll)
	s.ResolveToolForTest("p1", head, "Polling as proc 4, for up to 30m. You will be woken when it stops.\n\nFirst check:\n"+
		"11:58:10 1 of 3 updated\nNot met yet: until /deployed/ did not match.\nIf that is not what you meant to watch, kill_shell(4).", agent.KindPoll)
	settle(m)
	openBox(t, m, "kubectl rollout status")

	got := screen(m)
	if !strings.Contains(got, "11:58:10 1 of 3 updated") {
		t.Errorf("the first check is missing:\n%s", got)
	}
	for _, prose := range []string{"Polling as proc", "First check:", "Not met yet", "kill_shell"} {
		if strings.Contains(got, prose) {
			t.Errorf("the box draws the agent's instructions (%q):\n%s", prose, got)
		}
	}
}
