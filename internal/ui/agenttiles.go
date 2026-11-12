package ui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/subagent"
)

// A running name brightens under the sweep, which reads on the dark panel.
var rowShimmer = lipgloss.Blend1D(shimmerReach+2, lipgloss.Color("#d0d0d0"), lipgloss.Color("#ffffff"))[1:]

// tileRuns is the runs the running box draws: live runs, failures not yet acknowledged, and finishes
// still celebrating, each followed by its pair navigator, which is shown for as long as its driver is.
func (m *Model) tileRuns() []subagent.Run {
	var out []subagent.Run
	for _, run := range m.snap.Runs {
		_, celebrating := m.flourishing(run.ID)
		active := run.Live() || (run.State == subagent.Failed && !run.Seen) || celebrating
		if !active || run.Parent != 0 {
			continue
		}
		out = append(out, run)
		for _, nav := range m.snap.Runs {
			if nav.Parent == run.ID {
				out = append(out, nav)
			}
		}
	}
	return out
}

// runInk is a run row's dot colour, name paint and pose (hat, face, one-line face): muted while
// another run is viewed, bold while it is.
func (m *Model) runInk(run subagent.Run) (tone color.Color, pose []string, paint func(...string) string, dim bool) {
	dim = m.runView != nil && !m.viewing(run)
	tone = m.pillColor(run)
	text := plainStyle
	if dim {
		tone, text = mutedColor, dimStyle
	}
	if m.viewing(run) {
		text = text.Bold(true)
	}
	paint = text.Render
	if run.State == subagent.Running && !dim {
		paint = func(name ...string) string {
			return shimmer(strings.Join(name, ""), m.poseTick, headShimmerSpeed, text, rowShimmer)
		}
	}
	if rows := m.runPose(run); rows != nil {
		hat, face := runHatStyle(run.ID), aluxFace
		if dim {
			hat, face = text, text
		}
		pose = []string{hat.Render(rows[0]), face.Render(rows[1]), face.Render(strings.TrimPrefix(rows[1], " "))}
	}
	return tone, pose, paint, dim
}

// viewing reports whether the run view on screen is this run's.
func (m *Model) viewing(run subagent.Run) bool {
	return m.runView != nil && m.runView.id == run.ID
}

// tileTitle is the run's label and its agent, or the agent alone for a run with neither name nor task.
func tileTitle(run subagent.Run) string {
	if label := run.Label(); label != "" {
		return label + " · " + run.Agent
	}
	return run.Agent
}

// pillColor is the run's state; a run waiting on the user pulses like a waiting session's dot.
func (m *Model) pillColor(run subagent.Run) color.Color {
	if run.State == subagent.Waiting {
		return pulse(m.poseTick, waitingPulse)
	}
	return runColor(run.State)
}

func stepCount(run subagent.Run) string {
	if run.MaxSteps == 0 {
		return fmt.Sprintf("step %d", run.Step)
	}
	return fmt.Sprintf("%d/%d", run.Step, run.MaxSteps)
}

func (m *Model) runElapsed(run subagent.Run) time.Duration {
	if !run.Ended.IsZero() {
		return run.Ended.Sub(run.Started)
	}
	return m.now().Sub(run.Started)
}

// tileAge is an elapsed time in its largest unit, which is all a glance at a row wants.
func tileAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
