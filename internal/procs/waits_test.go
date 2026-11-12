package procs

import (
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/sandbox"
)

// testCheckInUnit is a check_in second for a test that waits out a check-in.
const testCheckInUnit = 100 * time.Millisecond

// woken is a registry whose wakes land on a channel, as the session's would.
func woken(t *testing.T) (*Registry, chan struct{}) {
	t.Helper()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	ch := make(chan struct{}, 8)
	r.OnExit = func() { ch <- struct{}{} }
	return r, ch
}

func background(t *testing.T, r *Registry, input map[string]any) (*Proc, string) {
	t.Helper()
	input["lifetime"] = "ongoing"
	got := text(t, r.runStart(context.Background(), call(t, "run_background", input), approving()))
	p, ok := r.Get(1)
	if !ok {
		t.Fatalf("the command was not started: %q", got)
	}
	return p, got
}

// news waits for the next wake that delivers something and returns it.
// A wake folded into one already taken still signals, so a signal can arrive with nothing queued.
func news(t *testing.T, r *Registry, ch chan struct{}) Exit {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-ch:
		case <-deadline:
			t.Fatal("nothing woke the agent")
		}
		switch exits := r.TakeExits(); len(exits) {
		case 0:
		case 1:
			return exits[0]
		default:
			t.Fatalf("woken with %+v, want one piece of news", exits)
		}
	}
}

func TestUntilWakesAtALineWrittenInPiecesAndLeavesTheProcessRunning(t *testing.T) {
	t.Parallel()
	r, ch := woken(t)
	p, _ := background(t, r, map[string]any{"check_in": 600, "until": "listening on",
		"command": `printf 'server lis'; sleep 0.3; printf '\033[32mtening\033[0m on :3000\n'; sleep 30`})

	e := news(t, r, ch)
	if e.Event != EventMatched || e.Matched != "server listening on :3000" {
		t.Fatalf("news = %+v; want a match on the reassembled, uncoloured line", e)
	}
	if p.Exited() || !strings.Contains(e.Message(), "still running") {
		t.Fatalf("a match stopped the process or did not say it runs on: %q", e.Message())
	}
}

func TestUntilSeesALineThatWasPrintedBeforeTheWakeWasSet(t *testing.T) {
	t.Parallel()
	r, ch := woken(t)
	p, _ := background(t, r, map[string]any{"command": "echo READY; sleep 30", "check_in": 600})
	waitFor(t, "the ready line", func() bool { return strings.Contains(p.Buffered(), "READY") })

	text(t, r.runBackground(context.Background(), call(t, "background_proc",
		map[string]any{"id": 1, "check_in": 600, "until": "READY"}), approving()))
	if e := news(t, r, ch); e.Event != EventMatched {
		t.Fatalf("news = %+v; want the unread ready line to match", e)
	}
}

func TestUntilMatchesALineTheBufferHasSinceDropped(t *testing.T) {
	t.Parallel()
	r, ch := woken(t)
	background(t, r, map[string]any{"check_in": 600, "until": "^FAILED",
		"command": `sleep 0.3; echo FAILED: TestX; head -c 200000 /dev/zero | tr '\0' 'x'; echo; sleep 30`})

	if e := news(t, r, ch); e.Matched != "FAILED: TestX" {
		t.Fatalf("news = %+v; want the match even though 200KB of output followed it", e)
	}
}

func TestACheckInWakesWithTheTailAndTheSilence(t *testing.T) {
	t.Parallel()
	r, ch := woken(t)
	r.checkInUnit = testCheckInUnit
	p, _ := background(t, r, map[string]any{"command": "echo progress; sleep 30", "check_in": 5, "until": "never"})

	e := news(t, r, ch)
	msg := e.Message()
	for _, want := range []string{"check-in", "still running", "last output", "progress", "background_proc(1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("check-in = %q, want it to contain %q", msg, want)
		}
	}
	if p.Exited() {
		t.Fatal("a check-in stopped the process")
	}
}

func TestAnExitBeforeTheMatchWakesOnlyWithTheExit(t *testing.T) {
	t.Parallel()
	r, ch := woken(t)
	background(t, r, map[string]any{"command": "echo nothing useful; exit 3", "check_in": 600, "until": "READY"})

	if e := news(t, r, ch); e.Event != "" || e.Status != "exit 3" {
		t.Fatalf("news = %+v, want just the exit", e)
	}
}

func TestBackgroundingAgainReplacesTheWake(t *testing.T) {
	t.Parallel()
	r, ch := woken(t)
	r.checkInUnit = testCheckInUnit
	background(t, r, map[string]any{"command": "sleep 30", "check_in": 5})
	text(t, r.runBackground(context.Background(), call(t, "background_proc", map[string]any{"id": 1, "check_in": 600}), approving()))

	select {
	case <-ch:
		t.Fatalf("the replaced 5s check-in still fired: %+v", r.TakeExits())
	case <-time.After(12 * testCheckInUnit):
	}
}

// armed starts command in the background with w as its wake, below the check_in floor the tools enforce.
// It arms only once ready has been printed, so a slow bash start under a loaded run cannot beat a short check-in.
func armed(t *testing.T, r *Registry, command, ready string, w Wake) *Proc {
	t.Helper()
	p, err := Start(r.Context(), command, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ready != "" {
		waitFor(t, ready, func() bool { return strings.Contains(p.Buffered(), ready) })
	}
	r.Adopt(p, "call_1")
	if !r.Arm(p, w) {
		t.Fatal("the wake was not armed")
	}
	return p
}

func TestARepeatingCheckInKeepsComingWithoutReArming(t *testing.T) {
	r, ch := woken(t)
	p := armed(t, r, "i=1; while :; do echo step-$i; i=$((i+1)); sleep 0.05; done", "step-1",
		Wake{CheckIn: 150 * time.Millisecond, Repeat: true})

	first := news(t, r, ch)
	second := news(t, r, ch)
	for i, e := range []Exit{first, second} {
		if e.Event != EventCheckIn || e.Next != 150*time.Millisecond || !strings.Contains(e.Output, "step-") {
			t.Fatalf("check-in %d = %+v, want a repeating check-in carrying new output", i+1, e)
		}
	}
	if strings.Contains(second.Output, "step-1\n") {
		t.Fatalf("the second check-in repeated output the first already delivered: %q", second.Output)
	}
	if msg := second.Message(); !strings.Contains(msg, "next check-in comes in") || strings.Contains(msg, "spent") {
		t.Fatalf("message = %q, want it to say the next one comes on its own", msg)
	}
	if p.Exited() {
		t.Fatal("a check-in stopped the process")
	}
}

func TestUnreadRepeatingCheckInsFoldIntoOne(t *testing.T) {
	t.Parallel()
	r, ch := woken(t)
	p := armed(t, r, "i=1; while :; do echo step-$i; i=$((i+1)); sleep 0.02; done", "step-1",
		Wake{CheckIn: 100 * time.Millisecond, Repeat: true})

	for range 3 {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal("the check-ins stopped coming")
		}
	}
	r.Arm(p, Wake{CheckIn: time.Hour})
	exits := r.TakeExits()
	if len(exits) != 1 {
		t.Fatalf("got %d pieces of news for one process, want the unread check-ins folded into one", len(exits))
	}
	var rest string
	waitFor(t, "output after the check-ins", func() bool { rest += p.Take(); return rest != "" })
	all := strings.Split(strings.TrimSuffix(exits[0].Output+rest, "\n"), "\n")
	for i, line := range all {
		if want := "step-" + strconv.Itoa(i+1); line != want {
			t.Fatalf("line %d is %q, want %q: the folded check-in lost output: %q", i+1, line, want, exits[0].Output)
		}
	}
}

func TestARepeatingWakeKeepsCheckingInAfterUntilMatches(t *testing.T) {
	r, ch := woken(t)
	armed(t, r, "echo READY; sleep 30", "READY", Wake{CheckIn: 150 * time.Millisecond, Until: regexp.MustCompile("READY"), Repeat: true})

	if e := news(t, r, ch); e.Event != EventMatched || !strings.Contains(e.Message(), "Check-ins carry on") {
		t.Fatalf("news = %+v, want the match saying check-ins carry on", e)
	}
	if e := news(t, r, ch); e.Event != EventCheckIn {
		t.Fatalf("news = %+v, want a check-in after the match", e)
	}
}

func TestBackgroundingAgainStopsARepeatingCheckIn(t *testing.T) {
	t.Parallel()
	r, ch := woken(t)
	p := armed(t, r, "sleep 30", "", Wake{CheckIn: 100 * time.Millisecond, Repeat: true})
	news(t, r, ch)

	r.Arm(p, Wake{CheckIn: time.Hour})
	r.TakeExits()
	select {
	case <-ch:
		t.Fatalf("the replaced repeating check-in still fired: %+v", r.TakeExits())
	case <-time.After(400 * time.Millisecond):
	}
}

// heads records what each tool call's box was left headed and coloured as.
func heads(t *testing.T) (agent.Callbacks, *[]string, *[]agent.ToolKind) {
	t.Helper()
	var labels []string
	var kinds []agent.ToolKind
	cb := approving()
	cb.OnToolResult = func(_, label, _ string, kind agent.ToolKind) {
		labels, kinds = append(labels, label), append(kinds, kind)
	}
	return cb, &labels, &kinds
}

func TestAPollIsDrawnAsAPollNamingItsIntervalStopConditionTimeoutAndWake(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	cb, labels, kinds := heads(t)
	r.runPoll(context.Background(), call(t, "poll", map[string]any{"command": "gh pr checks 12", "every": 30,
		"until": "pass|fail", "seconds": 1800, "check_in": 300, "repeat": true}), cb)
	if p, ok := r.Get(1); ok {
		t.Cleanup(func() { p.Kill(); <-p.Done() })
	}

	want := "poll every 30s until /pass|fail/, gives up after 30m: gh pr checks 12 · check-in every 5m"
	if len(*labels) != 1 || (*labels)[0] != want || (*kinds)[0] != agent.KindPoll {
		t.Fatalf("heads = %q kinds = %v, want %q as a poll", *labels, *kinds, want)
	}
}

func TestBackgroundingHeadsShowTheirWake(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	cb, labels, kinds := heads(t)
	r.runStart(context.Background(), call(t, "run_background", map[string]any{"command": "npm run dev",
		"check_in": 300, "until": "Ready", "lifetime": "ongoing"}), cb)
	r.runBackground(context.Background(), call(t, "background_proc", map[string]any{"id": 1, "check_in": 600,
		"repeat": true}), cb)

	want := []string{"$ npm run dev & · check-in 5m or /Ready/", "background proc 1 · check-in every 10m"}
	if strings.Join(*labels, "\n") != strings.Join(want, "\n") {
		t.Fatalf("heads = %q, want %q", *labels, want)
	}
	for _, k := range *kinds {
		if k != agent.KindBash {
			t.Fatalf("kinds = %v, want a shell command drawn as bash", *kinds)
		}
	}
}

func TestBackgroundingWithoutACheckInIsRefused(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	for name, res := range map[string]string{
		"run_background": text(t, r.runStart(context.Background(), call(t, "run_background", map[string]any{"command": "sleep 30"}), approving())),
		"poll":           text(t, r.runPoll(context.Background(), call(t, "poll", map[string]any{"command": "echo x", "until": "y"}), approving())),
	} {
		if !strings.Contains(res, "check_in is required") {
			t.Errorf("%s without check_in answered %q, want it refused", name, res)
		}
	}
	if len(r.Running()) != 0 {
		t.Fatal("a refused call started something")
	}
}

func startPoll(t *testing.T, r *Registry, s Poll) *Proc {
	t.Helper()
	p := StartPoll(r.Context(), s, nil)
	r.Adopt(p, "call_1")
	t.Cleanup(func() { r.KillAll() })
	return p
}

func waitDone(t *testing.T, p *Proc) {
	t.Helper()
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("the poll never stopped; output so far %q", p.Buffered())
	}
}

func TestAPollWithWhileStopsAtTheFirstChangeAndLogsOnlyChanges(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	count := filepath.Join(t.TempDir(), "n")
	p := startPoll(t, r, Poll{
		Check: `n=$(cat ` + count + ` 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + count +
			`; if [ $n -lt 4 ]; then echo pending; exit 8; else echo pass; fi`,
		Every: 20 * time.Millisecond, For: time.Minute, While: regexp.MustCompile("pending"),
	})
	waitDone(t, p)

	log := strings.TrimSpace(p.Buffered())
	lines := strings.Split(log, "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "pending [exit 8]") || !strings.HasSuffix(lines[1], "pass") {
		t.Fatalf("log = %q, want one line for the pending run of checks and one for pass", log)
	}
	if st := p.Status(); !strings.HasPrefix(st, "condition met") || !strings.Contains(st, "4 checks") {
		t.Fatalf("status = %q, want the condition met after 4 checks", st)
	}
}

func TestAPollStopsAfterTheCheckFailsRepeatedly(t *testing.T) {
	was := checkTimeout
	checkTimeout = 100 * time.Millisecond
	t.Cleanup(func() { checkTimeout = was })
	r := NewRegistry()
	p := startPoll(t, r, Poll{Check: "sleep 5", Every: 10 * time.Millisecond, For: time.Minute})
	waitDone(t, p)

	if st := p.Status(); !strings.Contains(st, "failed 3 times in a row") {
		t.Fatalf("status = %q, want it to stop after three failed checks", st)
	}
}

func TestAPollGivesUpAtItsLimit(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	p := startPoll(t, r, Poll{Check: "echo pending", Every: 50 * time.Millisecond, For: 300 * time.Millisecond,
		Until: regexp.MustCompile("pass")})
	waitDone(t, p)

	if st := p.Status(); !strings.HasPrefix(st, "gave up") {
		t.Fatalf("status = %q, want it to give up at its limit", st)
	}
}

func TestARegisteredPollReportsHowItEndedUnderTheIdItsCallStarted(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		poll Poll
		kill bool
		want Stop
	}{
		{"met", Poll{Check: "echo pass", Every: time.Second, For: time.Minute, Until: regexp.MustCompile("pass")}, false, StopMet},
		{"timed out", Poll{Check: "echo pending", Every: 50 * time.Millisecond, For: 200 * time.Millisecond, Until: regexp.MustCompile("pass")}, false, StopTimedOut},
		{"killed", Poll{Check: "echo pending", Every: time.Second, For: time.Minute, Until: regexp.MustCompile("pass")}, true, StopKilled},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			ended := make(chan [2]int, 1)
			r.OnPollEnd = func(id int, how Stop) { ended <- [2]int{id, int(how)} }
			p := startPoll(t, r, c.poll)
			if got := r.PollOf("call_1"); got != p.ID {
				t.Fatalf("PollOf(call_1) = %d, want %d", got, p.ID)
			}
			if c.kill {
				p.Kill()
			}
			select {
			case got := <-ended:
				if got != [2]int{p.ID, int(c.want)} {
					t.Errorf("OnPollEnd got id %d how %d, want id %d how %d", got[0], got[1], p.ID, c.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("OnPollEnd never fired")
			}
		})
	}
}

const ghOffline = `printf 'error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com\n'; exit 1`

// sequence is a check that runs answers[n-1] on its nth run, and the last answer from then on.
func sequence(t *testing.T, answers ...string) string {
	t.Helper()
	count := filepath.Join(t.TempDir(), "n")
	var b strings.Builder
	b.WriteString(`n=$(cat ` + count + ` 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + count + `; case $n in `)
	for i, a := range answers[:len(answers)-1] {
		b.WriteString(strconv.Itoa(i+1) + ") " + a + ";; ")
	}
	b.WriteString("*) " + answers[len(answers)-1] + ";; esac")
	return b.String()
}

// sleeps is a poll's sleeper that returns at once, recording each wait, and sleeps the machine asleep[n-1] in the nth.
func sleeps(asleep ...time.Duration) (func(context.Context, time.Duration) (time.Duration, bool), *[]time.Duration) {
	var got []time.Duration
	return func(ctx context.Context, d time.Duration) (time.Duration, bool) {
		got = append(got, d)
		var a time.Duration
		if len(got) <= len(asleep) {
			a = asleep[len(got)-1]
		}
		return a, ctx.Err() == nil
	}, &got
}

func TestAPollWokenFromSleepSaysSoChecksAtOnceAndRestartsItsOfflineBackoff(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	sleep, waits := sleeps(0, 0, 2*time.Hour)
	p := startPoll(t, r, Poll{
		Check: sequence(t, ghOffline, ghOffline, ghOffline, ghOffline, "echo pass"),
		Every: time.Minute, For: 10 * time.Minute, Until: regexp.MustCompile("pass"), sleep: sleep,
	})
	waitDone(t, p)

	if st := p.Status(); !strings.HasPrefix(st, "condition met after") || !strings.Contains(st, "awake (plus 2h asleep), 5 checks") {
		t.Fatalf("status = %q, want the condition met with the 2h asleep named apart; log %q", st, p.Buffered())
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, time.Minute}
	if !slices.Equal(*waits, want) {
		t.Errorf("waits = %v, want %v: the backoff starts again from every after the wake", *waits, want)
	}
	lines := logLines(p)
	if len(lines) != 4 || !strings.Contains(lines[1], "resumed after ~2h asleep") || !strings.Contains(lines[2], "back online") {
		t.Errorf("log = %q, want offline, resumed after ~2h asleep, back online, pass", lines)
	}
}

func logLines(p *Proc) []string { return strings.Split(strings.TrimSpace(p.Buffered()), "\n") }

func TestAWhilePollNeverTakesAnOfflineErrorAsItsAnswer(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	sleep, _ := sleeps()
	p := startPoll(t, r, Poll{
		Check: sequence(t, "echo pending; exit 8", ghOffline, ghOffline, "echo pending; exit 8", "echo pass"),
		Every: time.Second, For: time.Minute, While: regexp.MustCompile("pending"), sleep: sleep,
	})
	waitDone(t, p)

	if st := p.Status(); !strings.HasPrefix(st, "condition met") || !strings.Contains(st, "5 checks") {
		t.Fatalf("status = %q, want the condition met at the fifth check, not at the offline ones; log %q", st, p.Buffered())
	}
	lines := logLines(p)
	want := []string{"pending [exit 8]", "offline: error connecting to api.github.com", "back online after ", "pending [exit 8]", "pass"}
	if len(lines) != len(want) {
		t.Fatalf("log = %q, want %d lines", lines, len(want))
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Errorf("log line %d = %q, want it to contain %q", i+1, lines[i], w)
		}
	}
}

func TestAnOfflinePollSaysSoOnceEachWayAndBacksOff(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	sleep, waits := sleeps()
	dial := `echo 'Post "https://api.github.com/graphql": dial tcp: lookup api.github.com: no such host'; exit 1`
	p := startPoll(t, r, Poll{
		Check: sequence(t, ghOffline, dial, ghOffline, dial, ghOffline, "echo pass"),
		Every: 2 * time.Minute, For: time.Hour, Until: regexp.MustCompile("pass"), sleep: sleep,
	})
	waitDone(t, p)

	if st := p.Status(); !strings.HasPrefix(st, "condition met") || !strings.Contains(st, "6 checks") {
		t.Fatalf("status = %q, want the condition met at the sixth check; log %q", st, p.Buffered())
	}
	log := p.Buffered()
	if strings.Count(log, "offline:") != 1 || strings.Count(log, "back online after") != 1 || len(logLines(p)) != 3 {
		t.Errorf("log = %q, want one offline line, one back online line and the answer", log)
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	if !slices.Equal(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestAnUntilPollWhoseCheckCannotStartStopsAfterThreeFailures(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	sleep, _ := sleeps()
	s := Poll{Check: "echo pass", Every: time.Second, For: time.Hour, Until: regexp.MustCompile("pass"), sleep: sleep}
	p := StartPoll(r.Context(), s, &sandbox.Wrap{Args: []string{filepath.Join(t.TempDir(), "missing")}})
	r.Adopt(p, "call_1")
	t.Cleanup(func() { r.KillAll() })
	waitDone(t, p)

	if st := p.Status(); !strings.Contains(st, "failed 3 times in a row") || strings.Count(p.Buffered(), "check failed") != 3 {
		t.Fatalf("status = %q, log %q; want it stopped after three checks that could not start", st, p.Buffered())
	}
}

func TestUnreachableReplacesTheBuiltInOfflineTest(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	sleep, _ := sleeps()
	p := startPoll(t, r, Poll{
		Check: sequence(t, "echo 'VPN is down'", "echo pass"),
		Every: time.Second, For: time.Minute, Until: regexp.MustCompile("pass|down"),
		Unreachable: regexp.MustCompile("VPN is down"), sleep: sleep,
	})
	waitDone(t, p)
	if st := p.Status(); !strings.Contains(st, "2 checks") || !strings.Contains(p.Buffered(), "offline: VPN is down") {
		t.Fatalf("status = %q, log %q; want the VPN line taken as offline and pass as the answer", st, p.Buffered())
	}

	got := pollCall(t, r, map[string]any{"command": ghOffline, "while": "pending", "unreachable": "VPN is down", "background": false})
	if !strings.Contains(got, "condition met") || strings.Contains(got, "offline:") {
		t.Fatalf("result = %q, want gh's error taken as the answer once unreachable replaces the built-in test", got)
	}
	if got := pollCall(t, r, map[string]any{"command": "true", "unreachable": "(", "background": false}); !strings.Contains(got, "unreachable is not a valid Go regular expression") {
		t.Fatalf("result = %q, want a bad unreachable refused", got)
	}
}

func pollCall(t *testing.T, r *Registry, input map[string]any) string {
	t.Helper()
	return text(t, r.runPoll(context.Background(), call(t, "poll", input), approving()))
}

func TestAPollDefaultsToTheBackground(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	got := pollCall(t, r, map[string]any{"command": "echo pending", "every": 5, "until": "pass", "check_in": 600})

	p, ok := r.Get(1)
	if !ok || !strings.Contains(got, "Polling as proc 1") || r.KillHeld() != nil {
		t.Fatalf("result = %q, registered %v; want a backgrounded proc returned at once", got, ok)
	}
	if p.Exited() {
		t.Fatal("the backgrounded poll stopped without its condition")
	}
}

func TestABackgroundPollReturnsItsFirstCheckAndWhyItIsNotMet(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	got := pollCall(t, r, map[string]any{"command": "sleep 0.2; echo pending", "until": "pass", "check_in": 600})

	if !strings.Contains(got, "Polling as proc 1") || !strings.Contains(got, " pending\n") ||
		!strings.Contains(got, "Not met yet: until /pass/ did not match.") || !strings.Contains(got, "kill_shell(1)") {
		t.Fatalf("result = %q, want the first check's output and why until is not met", got)
	}
	if p, _ := r.Get(1); strings.Contains(p.Take(), "pending") {
		t.Fatal("the first check's output was left for the next check-in to repeat")
	}
}

func TestABackgroundPollMetOnItsFirstCheckReportsItFinishedAndQueuesNoNews(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	ended := make(chan struct{})
	r.OnPollEnd = func(int, Stop) { close(ended) }
	got := pollCall(t, r, map[string]any{"command": "sleep 0.2; echo pass", "until": "pass", "check_in": 600})

	if !strings.Contains(got, " pass\n") || !strings.Contains(got, "[proc 1 finished — condition met after") {
		t.Fatalf("result = %q, want the log and the condition met", got)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the poll never reported its end")
	}
	if exits := r.TakeExits(); len(exits) != 0 {
		t.Fatalf("exits = %+v, want none: the tool result already reported the finish", exits)
	}
}

func TestAForegroundPollReturnsItsLogWhenItStops(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	got := pollCall(t, r, map[string]any{"command": "echo pass", "until": "pass", "background": false})

	if !strings.Contains(got, "pass") || !strings.Contains(got, "condition met") {
		t.Fatalf("result = %q, want the log and the condition met", got)
	}
	if _, registered := r.Get(1); registered {
		t.Fatal("a foreground poll that finished in time was registered as a proc")
	}
}

func TestAForegroundPollPastTheLimitIsHeld(t *testing.T) {
	withForegroundLimit(t, 150*time.Millisecond)
	r := NewRegistry()
	t.Cleanup(func() { r.KillAll() })
	got := pollCall(t, r, map[string]any{"command": "echo pending", "until": "pass", "background": false})

	if !strings.Contains(got, "NOT killed") || !strings.Contains(got, "background_proc(1") {
		t.Fatalf("result = %q, want it held with the bash choices", got)
	}
	if held := r.KillHeld(); len(held) != 1 {
		t.Fatalf("%d held, want the overrunning poll held", len(held))
	}
}

func TestKillingAPollStopsItAndReportsKilled(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	p := startPoll(t, r, Poll{Check: "echo pending", Every: time.Hour, For: 2 * time.Hour,
		Until: regexp.MustCompile("pass")})
	waitFor(t, "the first check", func() bool { return strings.Contains(p.Buffered(), "pending") })

	p.Kill()
	waitDone(t, p)
	if st := p.Status(); st != "killed" {
		t.Fatalf("status = %q, want killed", st)
	}
}
