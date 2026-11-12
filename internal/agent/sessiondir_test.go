package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/mockapi"
)

func TestASessionDirCommandAsksInAutoModeEvenAsAPreapprovedStep(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		command string
		held    bool
	}{
		{"cat " + home + "/.cache/alux/sessions/workers/ALUX-1/prompt.json", true},
		{"echo '{}' > ~/.cache/alux/sessions/workers/ALUX-1/answer.json", true},
		{"ls $HOME/.cache/alux/sessions", true},
		{"cp x.json ${HOME}/.cache/alux/sessions/123.json", true},
		{"cd ~ && rm .cache/alux/sessions/123.json", true},
		{"ls ~/.cache/pip", false},
	} {
		a, _, r := sandboxedAgent(t, shellTurn("bash", tc.command), mockapi.Turn{Text: "done"})
		a.Allows = allows(guardian.Read, guardian.Edit, guardian.Network)
		a.PreapprovedStep = func(string) bool { return true }
		var reasons []string
		cb := (&recorder{}).callbacks()
		cb.Require = func(_ context.Context, _, _, reason string) bool { reasons = append(reasons, reason); return false }
		a.RunTurn(context.Background(), user("go"), cb)
		held := len(reasons) == 1 && strings.Contains(reasons[0], "session registry or worker channel")
		if _, ran := r["t1"]; held != tc.held || ran == tc.held || len(reasons) > 1 {
			t.Errorf("%s: asked %v, ran %v; want held=%v", tc.command, reasons, ran, tc.held)
		}
	}
}
