package ui

import (
	"image/color"
	"sort"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/lucasb-eyer/go-colorful"
)

// A tool tone signals in colour alone: nothing else about a box says which tool drew it. Two
// tones close enough to be mistaken for one another are a defect that nothing else in the suite
// can catch, and eyeballing is what put a tone ΔE 18.3 from its neighbour into the palette in the
// first place.
//
// The floor is CIE76 ΔE 25, the same number the input frame's three states are held to. It sits
// far above the just-noticeable difference of about 2.3 because these are never seen side by
// side: a reader sees one box now and recognises it against a memory of the last one.
const toneFloor = 25

// Distances are measured in CIE L*a*b* through go-colorful, the library lipgloss itself blends
// in, so the number here is the one the rendered pixels imply.
func labDistance(a, b color.Color) float64 {
	ca, _ := colorful.MakeColor(a)
	cb, _ := colorful.MakeColor(b)
	return ca.DistanceLab(cb) * 100
}

// Only tones fixed as hex are pinned. bash and mcp are ANSI named colours, which a terminal
// resolves against its own palette, so their rendered value is the user's to decide and a
// distance measured against a standard interpretation of them would assert an assumption rather
// than a property of this palette.
func fixedToolTones(t *testing.T) map[string]color.Color {
	t.Helper()
	ansi := map[agent.ToolKind]bool{agent.KindBash: true, agent.KindMCP: true}
	out := map[string]color.Color{}
	for kind, tone := range toolColor {
		if ansi[kind] {
			continue
		}
		out[string(kind)] = tone
	}
	return out
}

func TestEveryToolToneIsToldApartFromEveryOther(t *testing.T) {
	tones := fixedToolTones(t)
	var names []string
	for name := range tones {
		names = append(names, name)
	}
	sort.Strings(names)

	worst, worstPair := 1000.0, ""
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			d := labDistance(tones[names[i]], tones[names[j]])
			if d < worst {
				worst, worstPair = d, names[i]+" vs "+names[j]
			}
			if d < toneFloor {
				t.Errorf("%s vs %s: ΔE %.1f, below the floor of %d — the two boxes read as the same tool",
					names[i], names[j], d, toneFloor)
			}
		}
	}
	t.Logf("closest pair: %s at ΔE %.1f (floor %d)", worstPair, worst, toneFloor)
}
