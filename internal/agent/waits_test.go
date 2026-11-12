package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestSleepLoopsAreRecognisedAndOtherSleepsAreNot(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{`for i in $(seq 1 60); do s=$(gh pr checks 1 | grep -i "Cursor Bugbot" | cut -f2); case "$s" in pending|queued|"") sleep 30;; *) exit 0;; esac; done`, true},
		{`until docker ps | grep redis | grep -q "Up " && nc -z localhost 6379; do sleep 60; echo waiting; done`, true},
		{"for i in $(seq 1 20); do\n  m=$(curl -s localhost:8080/healthcheck)\n  [ \"$m\" = 200 ] && break\n  sleep 15\ndone", true},
		{`while true; do gh run view 1 --json status; sleep 10; done`, true},
		{`cd x && while ! curl -sf localhost:3000; do sleep 2; done`, true},

		{`gh run rerun 1; sleep 5; gh run view 1`, false},
		{`for f in *.go; do gofmt -l $f; done; sleep 1`, false},
		{`echo "while true; do sleep 1; done"`, false},
		{`jj describe -m 'procs: poll replaces for loops that sleep until done'`, false},
		{`go test -run TestSleepLoop ./...`, false},
		{"python3 - <<EOF\nwhile True:\n    time.sleep(1)\nEOF", false},
	} {
		if got := sleepLoop(tc.command); got != tc.want {
			t.Errorf("sleepLoop(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

func withPoll(a *Agent) {
	a.Tools = func() []anthropic.ToolUnionParam {
		return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{Name: pollTool}}}
	}
}

const loop = `while ! curl -sf localhost:3000; do sleep 2; done`

func TestASleepLoopIsHeldWhenTheAgentCanPoll(t *testing.T) {
	a, f, r := sandboxedAgent(t, shellTurn("bash", loop), mockapi.Turn{Text: "done"})
	withPoll(a)
	out := lastResult(t, a, &recorder{})
	if _, did := r["t1"]; did || !strings.HasPrefix(out, "Not run") || !strings.Contains(out, "poll") {
		t.Errorf("a sleep loop ran or was not pointed at poll: ran %v, %q", r, out)
	}
	if len(f.records) != 0 {
		t.Errorf("a rule's suggestion reached guardian's log: %v", f.records)
	}
}

func TestASleepLoopRunsWhenTheAgentHasNoPoll(t *testing.T) {
	a, _, r := sandboxedAgent(t, shellTurn("bash", loop), mockapi.Turn{Text: "done"})
	lastResult(t, a, &recorder{})
	if r["t1"] != "sentinel" {
		t.Errorf("an agent with no poll had its sleep loop held: ran %v", r)
	}
}

func TestALeadingSleepIsHeldOnlyWhileABackgroundCommandRuns(t *testing.T) {
	for _, tc := range []struct {
		command string
		live    bool
		held    bool
	}{
		{"sleep 30; tmux capture-pane -p -t acme:0.1", true, true},
		{"cd api && sleep 20; curl -s localhost:8080/healthcheck", true, true},
		{"sleep 30; tmux capture-pane -p -t acme:0.1", false, false},
		{"make build; sleep 5; gh run view 1", true, false},
		{`echo "sleep 30"`, true, false},
	} {
		a, _, r := sandboxedAgent(t, shellTurn("bash", tc.command), mockapi.Turn{Text: "done"})
		withPoll(a)
		a.Backgrounded = func() bool { return tc.live }
		out := lastResult(t, a, &recorder{})
		if _, ran := r["t1"]; ran == tc.held {
			t.Errorf("%q with a live background command %v: ran %v, answered %q; want held %v", tc.command, tc.live, ran, out, tc.held)
		}
	}
}

func TestADeclinedSleepLoopIsPutToThePersonAndRuns(t *testing.T) {
	a, _, r := sandboxedAgent(t, shellTurn("bash", loop), declineTurn("s1", "it is a one-off"), mockapi.Turn{Text: "done"})
	withPoll(a)
	cb := (&recorder{}).callbacks()
	var reasons []string
	cb.Require = func(_ context.Context, _, _, reason string) bool { reasons = append(reasons, reason); return true }
	a.RunTurn(context.Background(), user("go"), cb)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "sleep loop") || r["t2"] != "sentinel" {
		t.Errorf("asked %q, ran %v; want one prompt naming the sleep loop, then a sandboxed run", reasons, r)
	}
}
