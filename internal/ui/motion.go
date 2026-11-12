package ui

import (
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// Blending needs real RGB, so these tones are hexes standing in for the named colours beside them.

const (
	shimmerReach = 2 // runes either side of the highlight's centre
	shimmerGap   = 10
	// Heads run far longer than "working…", so their sweep moves twice as fast.
	headShimmerSpeed = 2
	flashFor         = 1200 * time.Millisecond
)

var (
	workingShimmer    = lipgloss.Blend1D(shimmerReach+1, lipgloss.Color("#6c6c6c"), lipgloss.Color("#eeeeee"))
	cancellingShimmer = lipgloss.Blend1D(shimmerReach+1, lipgloss.Color("#87875f"), lipgloss.Color("#ffaf5f"))

	flashRamp = lipgloss.Blend1D(12, lipgloss.Color("#87ff87"), lipgloss.Color("#5f5f5f"))

	waitingPulse = lipgloss.Blend1D(9, lipgloss.Color("#ffd75f"), lipgloss.Color("#5f4f00"))

	namedTone = map[color.Color]color.Color{
		lipgloss.Green: lipgloss.Color("#5fd75f"),
		lipgloss.Blue:  lipgloss.Color("#5f87ff"),
	}
)

// headShimmer brightens a tone towards white, skipping the stand-in so it never sits beside the real tone.
func headShimmer(tone color.Color) []color.Color {
	from := tone
	if hex, ok := namedTone[tone]; ok {
		from = hex
	}
	if from == nil {
		from = lipgloss.Color("#6c6c6c")
	}
	return lipgloss.Blend1D(shimmerReach+2, from, lipgloss.Color("#ffffff"))[1:]
}

// shimmer sweeps a highlight left to right across text, resting for shimmerGap ticks between passes.
func shimmer(text string, tick, speed int, base lipgloss.Style, ramp []color.Color) string {
	runes := []rune(text)
	head := (tick * speed) % (len(runes) + shimmerGap)
	var b strings.Builder
	for i, r := range runes {
		d := abs(i - head)
		if d > shimmerReach {
			b.WriteString(base.Render(string(r)))
			continue
		}
		b.WriteString(base.Foreground(ramp[shimmerReach-d]).Render(string(r)))
	}
	return b.String()
}

// pulse walks a ramp forth and back, one step a tick.
func pulse(tick int, ramp []color.Color) color.Color {
	period := 2 * (len(ramp) - 1)
	i := tick % period
	if i >= len(ramp) {
		i = period - i
	}
	return ramp[i]
}

// turnFlash is the frame's colour in the moment after a turn ends, and false once it has faded.
func (m *Model) turnFlash() (color.Color, bool) {
	if m.turnDone.IsZero() {
		return nil, false
	}
	elapsed := m.now().Sub(m.turnDone)
	if elapsed < 0 || elapsed >= flashFor {
		return nil, false
	}
	return flashRamp[int(elapsed*time.Duration(len(flashRamp))/flashFor)], true
}
