package subagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/paths"
)

// Routing names the models for guardian's small and medium tiers; large work stays on the agent's own model.
type Routing struct {
	Small  string `json:"small"`
	Medium string `json:"medium"`
}

// TierDefault is a run on the agent's own model because routing was off or guardian gave no answer.
const TierDefault = "default"

// LoadRouting reads {"routing": {"small": "...", "medium": "..."}}; absent or incomplete means off.
func LoadRouting() (Routing, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Routing{}, false
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return Routing{}, false
	}
	var settings struct {
		Routing *Routing `json:"routing"`
	}
	if json.Unmarshal(data, &settings) != nil || settings.Routing == nil {
		return Routing{}, false
	}
	r := *settings.Routing
	if r.Small == "" || r.Medium == "" {
		return Routing{}, false
	}
	return r, true
}

// Pick is one run's model, the tier that chose it, and guardian's scores for each tier.
type Pick struct {
	Model  string
	Tier   string
	Scores map[string]float64
}

// Tierer scores a task's size; false when it could not.
type Tierer func(ctx context.Context, task string) (map[string]float64, bool)

// pick routes an exploring run by guardian's likeliest tier, and keeps the agent's own model whenever it cannot.
func (d *Definition) pick(ctx context.Context, routing func() (Routing, bool), tier Tierer, text string) Pick {
	model := d.model()
	fallback := Pick{Model: model, Tier: TierDefault}
	if !d.Explore || routing == nil || tier == nil {
		return fallback
	}
	r, ok := routing()
	if !ok {
		return fallback
	}
	scores, ok := tier(ctx, text)
	if !ok {
		return fallback
	}
	top := guardian.TierLarge
	for _, t := range []string{guardian.TierSmall, guardian.TierMedium} {
		if scores[t] > scores[top] {
			top = t
		}
	}
	switch top {
	case guardian.TierSmall:
		model = r.Small
	case guardian.TierMedium:
		model = r.Medium
	}
	return Pick{Model: model, Tier: top, Scores: scores}
}

// tierText is what guardian judges a run's size from: the agent and the whole first prompt it is given.
func tierText(agent, prompt string) string {
	return "Agent: " + agent + "\n\n" + prompt
}
