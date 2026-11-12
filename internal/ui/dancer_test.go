package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/subagent"
	"github.com/charmbracelet/x/ansi"
)

func agentHarness(t *testing.T, width int, state subagent.State) *Model {
	t.Helper()
	m, s := harness(t, width, 24)
	roster := subagent.NewRoster(nil)
	s.Roster = roster
	s.ShowRunForTest(roster.AddForTest(subagent.Run{Agent: "research", Name: "code read", State: state, Task: "read the code",
		Step: 12, MaxSteps: 30}))
	m.sidebarOpen = true
	settle(m)
	return m
}

const runHead = "code read · research"

// ownHat is the hat the harness's one run wears.
func ownHat(m *Model) string { return runHat(m.snap.Runs[0].ID) }

// runRows is the run's two rows in the running box, empty when the run has none.
func runRows(m *Model) (string, string) {
	box, targets := m.sidebar()
	lines := strings.Split(ansi.Strip(box), "\n")
	for i, tg := range targets {
		if tg.run != 0 && !tg.scroll {
			return lines[i+1], lines[i+2]
		}
	}
	return "", ""
}

// finishing starts a subagent running against a clock the test moves, then ends it in state.
func finishing(t *testing.T, state subagent.State) (*Model, func(time.Duration)) {
	t.Helper()
	m := agentHarness(t, 100, subagent.Running)
	now := time.Now()
	m.now = func() time.Time { return now }
	roster(m).SetStateForTest(m.snap.Runs[0].ID, state)
	settle(m)
	return m, func(d time.Duration) { now = now.Add(d) }
}

func TestAFinishedSubagentLiftsItsHatAndLeavesTheBox(t *testing.T) {
	m, advance := finishing(t, subagent.Done)
	if hat, _ := runRows(m); !strings.Contains(hat, ownHat(m)) {
		t.Fatalf("a run that just finished shows %q, want its hat", hat)
	}
	advance(flourishStep)
	tick(m)
	if hat, _ := runRows(m); !strings.Contains(hat, aluxHatLifted) {
		t.Errorf("a beat after finishing the hat reads %q, want it lifted", hat)
	}
	top := m.transcriptTop()

	advance(flourishFor)
	tick(m)
	if box := plainBox(m); strings.Contains(box, runHead) {
		t.Errorf("after the window the run is still in the box:\n%s", box)
	}
	if !strings.Contains(screen(m), runHead) || m.transcriptTop() != top {
		t.Errorf("the transcript moved when the run left the box:\n%s", screen(m))
	}
}

func TestAFailedSubagentLooksKnockedBriefly(t *testing.T) {
	m, advance := finishing(t, subagent.Failed)
	hat, face := runRows(m)
	if !strings.Contains(hat, tipHat(ownHat(m), -1)) || !strings.Contains(face, aluxEyesSad) {
		t.Fatalf("a run that just failed shows %q over %q, want the knocked hat and sad eyes", hat, face)
	}

	advance(flourishFor)
	tick(m)
	if hat, face := runRows(m); strings.Contains(hat, tipHat(ownHat(m), -1)) || strings.Contains(face, aluxEyesSad) {
		t.Errorf("after the window the row still shows %q over %q", hat, face)
	}
}

func TestTheTranscriptHeadShowsNoAlux(t *testing.T) {
	for _, state := range []subagent.State{subagent.Running, subagent.Idle, subagent.Waiting} {
		m := agentHarness(t, 80, state)
		head := ansi.Cut(ansi.Strip(rowWith(t, m, runHead)), 0, m.mainWidth())
		if strings.Contains(head, ownHat(m)) {
			t.Errorf("%s: the transcript head %q shows him, who belongs in the running box", state, head)
		}
	}
}

func TestARunningSubagentLooksAround(t *testing.T) {
	m := agentHarness(t, 100, subagent.Running)
	gazes := map[string]bool{}
	held, longest := 0, 0
	prev := ""
	for range 10 * gazeHold {
		tick(m)
		hat, face := runRows(m)
		if !strings.Contains(hat, ownHat(m)) {
			t.Fatalf("a running subagent wears %q, want its hat upright", hat)
		}
		gazes[face] = true
		if face == prev {
			held++
		} else {
			held = 1
		}
		longest = max(longest, held)
		prev = face
	}
	if len(gazes) < 3 {
		t.Errorf("a running subagent looked only %d ways in %d ticks", len(gazes), 10*gazeHold)
	}
	if longest < gazeHold {
		t.Errorf("a running subagent never held a gaze for %d ticks; longest was %d", gazeHold, longest)
	}
}

func TestRunsSideBySideDoNotGlanceTogether(t *testing.T) {
	var a, b []string
	for tick := range 10 * gazeHold {
		a = append(a, gaze(1, tick))
		b = append(b, gaze(2, tick))
	}
	if slices.Equal(a, b) {
		t.Errorf("runs 1 and 2 look the same way on every tick: %v", a)
	}
}

func TestAFinishedSubagentShowsNoAlux(t *testing.T) {
	m := agentHarness(t, 100, subagent.Done)
	if hat, face := runRows(m); strings.ContainsAny(hat+face, "▅█") {
		t.Errorf("a run finished before the pane saw it live still shows %q over %q", hat, face)
	}
}

func TestAnIdleSubagentDozesUnderATippedHat(t *testing.T) {
	m := agentHarness(t, 100, subagent.Idle)
	faces := map[string]bool{}
	for range 4 * aluxHold {
		tick(m)
		hat, face := runRows(m)
		if !strings.Contains(hat, tipHat(ownHat(m), 1)) {
			t.Fatalf("an idle run wears %q, want the tipped hat", hat)
		}
		faces[face] = true
	}
	if len(faces) < 2 {
		t.Errorf("an idle run never stirred: %v", faces)
	}
}

func TestAWaitingSubagentWaves(t *testing.T) {
	m := agentHarness(t, 100, subagent.Waiting)
	seen := map[string]bool{}
	for range 2 * aluxBeat {
		tick(m)
		_, face := runRows(m)
		for _, f := range []string{aluxWaveUp, aluxWaveDown} {
			if strings.Contains(face, f) {
				seen[f] = true
			}
		}
	}
	if len(seen) != 2 {
		t.Errorf("a run waiting on the user did not wave: saw %v", seen)
	}
}

func TestEveryRunPoseIsTwoRowsOfOneWidth(t *testing.T) {
	m := agentHarness(t, 100, subagent.Running)
	run := m.snap.Runs[0]
	m.finished[run.ID] = m.now()
	for _, state := range []subagent.State{subagent.Running, subagent.Waiting, subagent.Idle, subagent.Done, subagent.Failed} {
		run.State = state
		for f := range 4 * aluxHold {
			m.poseTick = f
			rows := m.runPose(run)
			if len(rows) != 2 {
				t.Fatalf("%s frame %d has %d rows", state, f, len(rows))
			}
			for _, r := range rows {
				if w := ansi.StringWidth(r); w != aluxWide {
					t.Fatalf("%s frame %d has a row %q %d cells wide", state, f, r, w)
				}
			}
		}
	}
}

func TestTheHatSitsBetweenTheDotAndTheNameAndTheKindBesideTheFace(t *testing.T) {
	m := agentHarness(t, 100, subagent.Running)
	hat, face := runRows(m)
	if !strings.Contains(hat, "● "+ownHat(m)+" ") {
		t.Errorf("the first row reads %q", hat)
	}
	if strings.Contains(face, stepCount(m.snap.Runs[0])) {
		t.Errorf("the second row still counts steps: %q", face)
	}
	if !strings.Contains(face, "▘  research") {
		t.Errorf("the second row does not put the kind beside his face: %q", face)
	}
}

func TestTheAluxMutesWithItsRow(t *testing.T) {
	m, ids := tiledHarness(t, 100, running("first", 20, 40), running("second", 20, 40))
	m.openRun(ids[1])
	row := agentLine(t, m, ids[0])
	if hat, name := cellOf(t, row, strings.TrimSpace(runHat(ids[0]))), cellOf(t, row, "first"); !sameColor(hat.Style.Fg, name.Style.Fg) {
		t.Errorf("a muted row's hat is not drawn like its name")
	}
}
