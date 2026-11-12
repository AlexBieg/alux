package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/anthropics/anthropic-sdk-go"
)

func routed() (Routing, bool) { return Routing{Small: "small-model", Medium: "medium-model"}, true }

func scoring(scores map[string]float64) (Tierer, func() []string) {
	var mu sync.Mutex
	var asked []string
	return func(_ context.Context, text string) (map[string]float64, bool) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, text)
			return scores, scores != nil
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), asked...)
		}
}

func exploring() *Definition {
	def := echo(false)
	def.Explore = true
	return def
}

func TestGuardiansLikeliestTierPicksTheModel(t *testing.T) {
	for _, tc := range []struct {
		scores map[string]float64
		model  string
		tier   string
	}{
		{map[string]float64{"small": 0.6, "medium": 0.3, "large": 0.1}, "small-model", "small"},
		{map[string]float64{"small": 0.1, "medium": 0.6, "large": 0.3}, "medium-model", "medium"},
		{map[string]float64{"small": 0.1, "medium": 0.3, "large": 0.6}, "mock", "large"},
	} {
		m, server := manager(t, exploring(), mockapi.Turn{Text: "done"})
		m.Routing = routed
		m.Tier, _ = scoring(tc.scores)
		logged := capture(m)

		if _, err := m.Dispatch(context.Background(), "echo", "x", "", agent.Callbacks{}); err != nil {
			t.Fatal(err)
		}
		if got := server.LastRequest()["model"]; got != tc.model {
			t.Errorf("%s: request went to %v, want %s", tc.tier, got, tc.model)
		}
		rec := logged()[0]
		if rec.Model != tc.model || rec.Tier != tc.tier || rec.TierScores[tc.tier] != 0.6 {
			t.Errorf("%s: logged %s/%s/%v", tc.tier, rec.Model, rec.Tier, rec.TierScores)
		}
		want := shortModel(tc.model) + " (" + tc.tier + ")"
		if head := m.Roster.Runs()[0].Headline(); !strings.Contains(head, want) {
			t.Errorf("%s: headline = %q, want it to name %q", tc.tier, head, want)
		}
	}
}

func TestARunGuardianCannotScoreStaysOnTheAgentsOwnModel(t *testing.T) {
	m, server := manager(t, exploring(), mockapi.Turn{Text: "done"})
	m.Routing = routed
	m.Tier, _ = scoring(nil)
	logged := capture(m)

	if _, err := m.Dispatch(context.Background(), "echo", "x", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if got := server.LastRequest()["model"]; got != "mock" {
		t.Errorf("request went to %v, want the agent's own model", got)
	}
	if rec := logged()[0]; rec.Tier != TierDefault || rec.TierScores != nil {
		t.Errorf("logged %s/%v, want default with no scores", rec.Tier, rec.TierScores)
	}
	if head := m.Roster.Runs()[0].Headline(); strings.Contains(head, "(") {
		t.Errorf("headline = %q, want no tier on a run routing did not choose", head)
	}
}

func TestAnAgentThatDoesNotExploreIsNeverScored(t *testing.T) {
	m, server := manager(t, echo(false), mockapi.Turn{Text: "done"})
	m.Routing = routed
	tier, asked := scoring(map[string]float64{"small": 1})
	m.Tier = tier

	if _, err := m.Dispatch(context.Background(), "echo", "x", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if got := server.LastRequest()["model"]; got != "mock" {
		t.Errorf("request went to %v", got)
	}
	if n := len(asked()); n != 0 {
		t.Errorf("guardian was asked %d times", n)
	}
}

func TestASkillRunIsScoredOnItsBoundBriefNotThePointerTask(t *testing.T) {
	def := probing(t)
	def.Explore = true
	m, _ := manager(t, def, note("n1"), note("n2"), finish("f1"), mockapi.Turn{Text: "Checked."})
	m.Routing = routed
	tier, asked := scoring(map[string]float64{"small": 0.1, "medium": 0.8, "large": 0.1})
	m.Tier = tier
	logged := capture(m)

	dispatchProbe(t, m)
	got := asked()
	if len(got) != 1 || !strings.Contains(got[0], "target = checkout flow") || !strings.HasPrefix(got[0], "Agent: echo") {
		t.Fatalf("guardian was asked %q, want the agent and the bound brief", got)
	}
	if rec := logged()[0]; rec.Inputs["target"] != "checkout flow" {
		t.Errorf("logged inputs %v, want the skill's bound inputs", rec.Inputs)
	}
}

func TestLoadRoutingTurnsOffUnlessBothTiersAreNamed(t *testing.T) {
	for _, tc := range []struct {
		settings string
		want     Routing
		ok       bool
	}{
		{`{"routing": {"small": "s", "medium": "m"}}`, Routing{Small: "s", Medium: "m"}, true},
		{`{"routing": {"medium": "m"}}`, Routing{}, false},
		{`{"routing": {"small": "s"}}`, Routing{}, false},
		{`{"routing": {"cheap": "c", "rate": 0.2}}`, Routing{}, false},
		{`{"vault": "x"}`, Routing{}, false},
		{`{"routing": `, Routing{}, false},
		{"", Routing{}, false},
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if tc.settings != "" {
			dir := filepath.Join(home, paths.Config)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.settings), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got, ok := LoadRouting(); got != tc.want || ok != tc.ok {
			t.Errorf("%q: LoadRouting = %+v, %v; want %+v, %v", tc.settings, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOnlyResearchCoderAndPairExplore(t *testing.T) {
	want := map[string]bool{"research": true, "coder": true, "pair": true}
	root := t.TempDir()
	for _, def := range []*Definition{
		Research(), coderDef(root, nil), pairedCoderDef(root, nil), Navigator(), Verifier(),
		Reviewer([]anthropic.ToolUnionParam{strict("pr_detail", "x", map[string]any{}, nil)}, nil),
	} {
		if def.Explore != want[def.Name] {
			t.Errorf("%s explores = %v", def.Name, def.Explore)
		}
	}
}

func TestTheHeadlineNamesTheModel(t *testing.T) {
	run := Run{Agent: "research", Model: "claude-sonnet-5-5", State: Done}
	if head := run.Headline(); !strings.HasPrefix(head, "research · sonnet-5-5 · done") {
		t.Errorf("headline = %q", head)
	}
}
