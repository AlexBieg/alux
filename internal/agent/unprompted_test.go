package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
)

func TestOnUnpromptedHearsOnlyAnEscalationGuardianPassedWithNobodyAsked(t *testing.T) {
	for _, tc := range []struct {
		name      string
		turn      mockapi.Turn
		policy    *Rules
		noSandbox bool
		want      []string
	}{
		{"escalated and passed", escalatedTurn("bash", "git push"), nil, false, []string{"git push"}},
		{"sandboxed", shellTurn("bash", "git push"), nil, false, nil},
		{"no sandbox to leave", shellTurn("bash", "git push"), nil, true, nil},
		{"escalated and asked by a rule", escalatedTurn("bash", "echo hi"),
			&Rules{Default: VerdictAllow, Bash: []Rule{{Pattern: "echo", Verdict: VerdictAsk, Reason: "a rule asks"}}}, false, nil},
	} {
		a, _, r := sandboxedAgent(t, tc.turn, mockapi.Turn{Text: "done"})
		if tc.policy != nil {
			a.Policy = tc.policy
		}
		if tc.noSandbox {
			a.Sandbox = nil
		}
		var heard []string
		a.OnUnprompted = func(command string) { heard = append(heard, command) }
		cb := (&recorder{}).callbacks()
		cb.Require = func(context.Context, string, string, string) bool { return true }
		a.RunTurn(context.Background(), user("go"), cb)
		if _, did := r["t1"]; !did {
			t.Fatalf("%s: the command did not run", tc.name)
		}
		if !slices.Equal(heard, tc.want) {
			t.Errorf("%s: heard %q, want %q", tc.name, heard, tc.want)
		}
	}
}
