package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/taint"
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

// A PHI read must take network away from auto, or curl could carry the PHI out unasked.
func TestAPHIReadDropsAutoToLocal(t *testing.T) {
	s, _ := newTestSession(t)
	s.SetMode(ModeAuto)
	if !s.Allows(guardian.Network) {
		t.Fatal("auto does not allow network")
	}
	taint.Set()
	t.Cleanup(taint.ResetForTest)
	if s.EffectiveMode() != ModeLocal || s.Allows(guardian.Network) || !s.Allows(guardian.Record) {
		t.Errorf("after a PHI read: mode %s, allowed %v", s.EffectiveMode(), s.Allowed())
	}
}

var mainCriteria = guardian.Criteria{Agent: "main", Allowed: []guardian.Category{guardian.Read, guardian.Build, guardian.Edit, guardian.Record}, Threshold: 0.26}

// guardianSession runs one shell command past a model stand-in answering probs, logging to a temp file.
func guardianSession(t *testing.T, probs []float64) (*Session, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := make([]string, len(probs))
		for i, p := range probs {
			parts[i] = fmt.Sprintf("%q: %g", fmt.Sprint(i), p)
		}
		fmt.Fprintf(w, `{"answers": {"does": {"probabilities": {%s}}}}`, strings.Join(parts, ", "))
	}))
	t.Cleanup(srv.Close)
	log := filepath.Join(t.TempDir(), "verdicts.jsonl")
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo guardian-ran"}}}},
		mockapi.Turn{Text: "done"},
	)
	s.agent.Screen = guardian.Gate{Client: guardian.New(guardian.Conf{URL: srv.URL, Model: "d3:2b", Log: log}, func(string) {}), Criteria: mainCriteria}
	return s, log
}

// read build edit record rewrite destroy network system
var (
	pushes  = []float64{0.01, 0.02, 0.01, 0.02, 0.01, 0.0, 0.92, 0.01}
	commits = []float64{0.02, 0.05, 0.03, 0.86, 0.02, 0.0, 0.01, 0.01}
)

func logged(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no verdict log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d log lines for one command, want 1:\n%s", len(lines), data)
	}
	var e map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	return e
}

func ran(s *Session) bool {
	for _, item := range toolItems(s.Snapshot()) {
		if strings.Contains(item.Body, "guardian-ran") {
			return true
		}
	}
	return false
}

// Auto mode approves everything, but a stopped command still asks; letting it run is a misclassification.
func TestAStoppedCommandAsksInAutoModeAndAnOverrideIsLoggedAsWrong(t *testing.T) {
	s, log := guardianSession(t, pushes)
	s.Submit("go")
	waitUntil(t, "the approval prompt", func(sn Snapshot) bool { return sn.Pending != "" }, s)
	if body := s.Snapshot().PendingBody; !strings.Contains(body, "network") {
		t.Errorf("the prompt does not say what guardian read it as: %q", body)
	}
	s.Approve(true)
	settled(t, s, "done")

	if !ran(s) {
		t.Fatal("an approved command did not run")
	}
	e := logged(t, log)
	if e["agent"] != "main" || e["decision"] != "approved" || e["human"] != "safe" || e["misclassified"] != true {
		t.Errorf("override not logged as a misclassification: %v", e)
	}
	var shown bool
	for _, item := range toolItems(s.Snapshot()) {
		shown = shown || (item.Guardian.Flagged && item.Guardian.Top == guardian.Network)
	}
	if !shown {
		t.Error("the box does not carry the verdict")
	}
}

func TestDecliningAStoppedCommandIsLoggedAsRight(t *testing.T) {
	s, log := guardianSession(t, pushes)
	s.Submit("go")
	waitUntil(t, "the approval prompt", func(sn Snapshot) bool { return sn.Pending != "" }, s)
	s.Approve(false)
	settled(t, s, "done")

	if ran(s) {
		t.Fatal("a declined command ran")
	}
	if e := logged(t, log); e["decision"] != "declined" || e["human"] != "unsafe" || e["misclassified"] != false {
		t.Errorf("decline not logged as a correct stop: %v", e)
	}
}

// A commit is inside the main agent's work, so it runs unasked in local mode.
func TestWorkInsideTheCriteriaRunsUnasked(t *testing.T) {
	s, log := guardianSession(t, commits)
	s.Submit("go")
	settled(t, s, "done")
	if !ran(s) {
		t.Fatal("a command inside the criteria did not run")
	}
	if e := logged(t, log); e["decision"] != "passed" || e["human"] != nil {
		t.Errorf("unreviewed command given a label: %v", e)
	}
}

// guardian down asks rather than running unchecked.
func TestACommandGuardianCannotJudgeAsks(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "echo guardian-ran"}}}},
		mockapi.Turn{Text: "done"},
	)
	s.agent.Screen = guardian.Gate{Client: guardian.New(guardian.Conf{URL: "http://127.0.0.1:1", Model: "m"}, func(string) {}), Criteria: mainCriteria}
	s.Submit("go")
	waitUntil(t, "the approval prompt", func(sn Snapshot) bool { return sn.Pending != "" }, s)
	s.Approve(false)
	settled(t, s, "done")
	if ran(s) {
		t.Fatal("an unjudged, declined command ran")
	}
}

var _ agent.Screener = passAll{}
