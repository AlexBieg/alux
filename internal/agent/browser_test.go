package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/taint"
)

const (
	fakeNetworkJSON = `{"success":true,"data":{"requests":[{"method":"POST","url":"http://localhost:3000/api/save","status":201,"resourceType":"Fetch","headers":{"a":"b"},"postData":"[{\"firstName\":\"Ada\"}]"},{"method":"GET","url":"http://localhost:3000/api/save?x","resourceType":"Fetch"}]}}`
	fakeBoxJSON     = `{"success":true,"data":{"x":10,"y":20,"width":30,"height":10}}`
	fakeLink        = `- link "Learn more" [ref=e1]`
)

// A stand-in for agent-browser that logs argv, remembers what was opened, and on click goes where next says.
const fakeAgentBrowser = `#!/bin/sh
dir="$(dirname "$0")"
echo "$*" >> "$dir/log"
shift 2
case "$1" in
open) echo "$2" > "$dir/state" ;;
click) [ -f "$dir/next" ] && cp "$dir/next" "$dir/state" ;;
get) if [ -f "$dir/state" ]; then cat "$dir/state"; else echo about:blank; fi ;;
snapshot) if [ -f "$dir/page" ]; then cat "$dir/page"; else echo '` + fakeLink + `'; fi ;;
read) echo '# Clients'; echo 'Ada assigned PHQ-9' ;;
--json) case "$2" in
	network) echo '` + fakeNetworkJSON + `' ;;
	*) echo '` + fakeBoxJSON + `' ;;
	esac ;;
esac
exit 0
`

// fakeBrowserRun answers a command as fakeAgentBrowser would, without starting a process.
func fakeBrowserRun(cmd *exec.Cmd) error {
	dir, args := filepath.Dir(cmd.Path), cmd.Args[1:]
	logFile, err := os.OpenFile(filepath.Join(dir, "log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(logFile, strings.Join(args, " "))
	if closeErr := logFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	arg := func(i int) string {
		if i+2 < len(args) {
			return args[i+2]
		}
		return ""
	}
	file := func(name, otherwise string) string {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return string(data)
		}
		return otherwise
	}
	var out string
	switch arg(0) {
	case "open":
		err = os.WriteFile(filepath.Join(dir, "state"), []byte(arg(1)+"\n"), 0o644)
	case "click":
		if next, readErr := os.ReadFile(filepath.Join(dir, "next")); readErr == nil {
			err = os.WriteFile(filepath.Join(dir, "state"), next, 0o644)
		}
	case "get":
		out = file("state", "about:blank\n")
	case "snapshot":
		out = file("page", fakeLink+"\n")
	case "read":
		out = "# Clients\nAda assigned PHQ-9\n"
	case "--json":
		out = fakeBoxJSON + "\n"
		if arg(1) == "network" {
			out = fakeNetworkJSON + "\n"
		}
	}
	if _, writeErr := io.WriteString(cmd.Stdout, out); err == nil {
		err = writeErr
	}
	return err
}

type fakeBrowserRig struct {
	dir   string
	asked []ask.Question
}

func browserRig(t *testing.T, allow ...string) *fakeBrowserRig {
	t.Helper()
	rig := &fakeBrowserRig{dir: t.TempDir()}
	binary := filepath.Join(rig.dir, "agent-browser")
	if err := os.WriteFile(binary, []byte(fakeAgentBrowser), 0o755); err != nil {
		t.Fatal(err)
	}
	run := browserRun
	browserRun = fakeBrowserRun
	t.Cleanup(func() { browserRun = run })
	settings, _ := json.Marshal(map[string]any{"browser": map[string]any{"binary": binary, "allow": allow}})
	writeWebSettings(t, string(settings))
	freshBrowserState(t)
	return rig
}

func freshBrowserState(t *testing.T) {
	t.Helper()
	clean := func() {
		browserOnce.Lock()
		clear(browserOnce.set)
		browserOnce.Unlock()
		browserSessions.Lock()
		clear(browserSessions.open)
		browserSessions.Unlock()
		taint.ResetForTest()
	}
	clean()
	t.Cleanup(clean)
}

func (r *fakeBrowserRig) answering(label string) Callbacks {
	return Callbacks{Ask: func(_ context.Context, q ask.Question) *ask.Answer {
		r.asked = append(r.asked, q)
		if label == "" {
			return nil
		}
		return &ask.Answer{Label: label}
	}}
}

func (r *fakeBrowserRig) log(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(r.dir, "log"))
	return string(data)
}

func browse(t *testing.T, handlers map[string]Handler, cb Callbacks, in map[string]any) (string, bool) {
	t.Helper()
	return resultText(t, handlers[browserTool](context.Background(), webCall(t, browserTool, in), cb))
}

func TestABrowserWildcardCoversSubdomainsButNotTheDomainItself(t *testing.T) {
	entries := []string{"*.example.com", "exact.test"}
	for host, want := range map[string]bool{
		"a.example.com":      true,
		"a.b.example.com":    true,
		"A.Example.COM":      true,
		"a.example.com:8443": true,
		"a.example.com.":     true,
		"example.com":        false,
		"badexample.com":     false,
		"example.com.evil":   false,
		"exact.test":         true,
		"EXACT.test:80":      true,
		"sub.exact.test":     false,
	} {
		if got := hostAllowed(host, entries); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLocalhostOpensWithoutAsking(t *testing.T) {
	rig := browserRig(t)
	cb := rig.answering(browserCancel)
	for _, url := range []string{"http://localhost:3000/", "http://127.0.0.1:8080/x", "http://[::1]:3000/"} {
		if text, isErr := browse(t, BrowserHandlers(), cb, map[string]any{"action": "open", "url": url}); isErr {
			t.Errorf("open %s: %s", url, text)
		}
	}
	taint.Set()
	if text, isErr := browse(t, SubagentBrowserHandlers(), cb, map[string]any{"action": "open", "url": "http://localhost:3000/"}); isErr {
		t.Errorf("localhost refused under taint: %s", text)
	}
	if len(rig.asked) != 0 {
		t.Errorf("asked %d times about localhost", len(rig.asked))
	}
}

func TestTheVerifierReminderComesOnceATurnOnTheFirstCallThatWorked(t *testing.T) {
	browserRig(t)
	cb := Callbacks{browserNudged: &atomic.Bool{}}
	handlers := BrowserHandlers()
	if text, _ := browse(t, handlers, cb, map[string]any{"action": "open", "url": "ftp://localhost/"}); strings.Contains(text, browserNudge) {
		t.Errorf("reminder on a failed call: %s", text)
	}
	open := map[string]any{"action": "open", "url": "http://localhost:3000/"}
	if text, _ := browse(t, handlers, cb, open); !strings.Contains(text, browserNudge) {
		t.Errorf("no reminder on the first call that worked: %s", text)
	}
	if text, _ := browse(t, handlers, cb, open); strings.Contains(text, browserNudge) {
		t.Errorf("reminder repeated in the same turn: %s", text)
	}
}

func TestAllowingOnceAsksOnceAndSavesNothing(t *testing.T) {
	rig := browserRig(t)
	cb := rig.answering(browserAllowOnce)
	for range 2 {
		if text, isErr := browse(t, BrowserHandlers(), cb, map[string]any{"action": "open", "url": "https://docs.example.com/a"}); isErr {
			t.Fatalf("open: %s", text)
		}
	}
	if len(rig.asked) != 1 {
		t.Fatalf("asked %d times, want 1", len(rig.asked))
	}
	var labels []string
	for _, o := range rig.asked[0].Options {
		labels = append(labels, o.Label)
	}
	if want := "Allow once|Always allow docs.example.com|Always allow *.example.com|Cancel"; strings.Join(labels, "|") != want {
		t.Errorf("options = %v, want %s", labels, want)
	}
	path, _ := browserDomainsPath()
	if _, err := os.Stat(path); err == nil {
		t.Error("allowing once wrote the allowlist file")
	}
}

func TestAlwaysAllowingSurvivesAReload(t *testing.T) {
	rig := browserRig(t)
	if text, isErr := browse(t, BrowserHandlers(), rig.answering("Always allow *.example.com"),
		map[string]any{"action": "open", "url": "https://a.example.com/"}); isErr {
		t.Fatalf("open: %s", text)
	}
	path, _ := browserDomainsPath()
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"*.example.com"`) {
		t.Fatalf("allowlist file = %q, %v", data, err)
	}
	browserOnce.Lock()
	clear(browserOnce.set)
	browserOnce.Unlock()
	if text, isErr := browse(t, SubagentBrowserHandlers(), Callbacks{}, map[string]any{"action": "open", "url": "https://b.c.example.com/"}); isErr {
		t.Errorf("a saved wildcard did not allow a fresh handler: %s", text)
	}
	if len(rig.asked) != 1 {
		t.Errorf("asked %d times, want 1", len(rig.asked))
	}
}

func TestCancellingRefusesWithoutOpening(t *testing.T) {
	rig := browserRig(t)
	for _, label := range []string{browserCancel, ""} {
		text, isErr := browse(t, BrowserHandlers(), rig.answering(label), map[string]any{"action": "open", "url": "https://evil.test/"})
		if !isErr || !strings.Contains(text, "declined") {
			t.Errorf("answer %q: %v %s", label, isErr, text)
		}
	}
	if strings.Contains(rig.log(t), "evil.test") {
		t.Errorf("the CLI was asked to open a cancelled host:\n%s", rig.log(t))
	}
}

func TestASubagentIsRefusedAnUnallowedHostWithoutAsking(t *testing.T) {
	rig := browserRig(t)
	text, isErr := browse(t, SubagentBrowserHandlers(), rig.answering(browserAllowOnce), map[string]any{"action": "open", "url": "https://evil.test/"})
	if !isErr || !strings.Contains(text, "the user must allow it first") {
		t.Errorf("subagent open: %v %s", isErr, text)
	}
	if len(rig.asked) != 0 || strings.Contains(rig.log(t), "evil.test") {
		t.Errorf("asked %d times; log:\n%s", len(rig.asked), rig.log(t))
	}
}

func TestPHIMakesAnAllowedHostAskEveryTime(t *testing.T) {
	rig := browserRig(t, "example.com")
	taint.Set()
	cb := rig.answering(browserThisTime)
	for range 2 {
		if text, isErr := browse(t, BrowserHandlers(), cb, map[string]any{"action": "open", "url": "https://example.com/"}); isErr {
			t.Fatalf("open: %s", text)
		}
	}
	if len(rig.asked) != 2 {
		t.Errorf("asked %d times under taint, want 2", len(rig.asked))
	}
	if labels := len(rig.asked[0].Options); labels != 2 {
		t.Errorf("a tainted prompt offers %d options, want this time or cancel", labels)
	}
	text, isErr := browse(t, SubagentBrowserHandlers(), cb, map[string]any{"action": "open", "url": "https://example.com/"})
	if !isErr || !strings.Contains(text, "read something sensitive") {
		t.Errorf("subagent under taint: %v %s", isErr, text)
	}
}

func TestAClickThatLandsOffTheAllowlistIsClearedAndRefused(t *testing.T) {
	rig := browserRig(t)
	if err := os.WriteFile(filepath.Join(rig.dir, "next"), []byte("https://evil.test/landing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, handlers := range map[string]map[string]Handler{"main": BrowserHandlers(), "subagent": SubagentBrowserHandlers()} {
		os.Remove(filepath.Join(rig.dir, "log"))
		cb := rig.answering(browserCancel)
		if text, isErr := browse(t, handlers, cb, map[string]any{"action": "open", "url": "http://localhost:3000/"}); isErr {
			t.Fatalf("%s open: %s", name, text)
		}
		text, isErr := browse(t, handlers, cb, map[string]any{"action": "click", "ref": "e1"})
		if !isErr || !strings.Contains(text, "about:blank") {
			t.Errorf("%s click: %v %s", name, isErr, text)
		}
		log := rig.log(t)
		if !strings.Contains(log, "click @e1") || !strings.HasSuffix(strings.TrimSpace(log), "open about:blank") {
			t.Errorf("%s log:\n%s", name, log)
		}
	}
}

func TestAPageChangingActionReturnsTheSnapshotAfterIt(t *testing.T) {
	rig := browserRig(t)
	text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "click", "ref": "e1"})
	if isErr || !strings.Contains(text, "<"+untrustedWeb+">\n- link \"Learn more\" [ref=e1]") {
		t.Errorf("click: %v %s", isErr, text)
	}
	log := rig.log(t)
	if click, snap := strings.Index(log, "click @e1"), strings.LastIndex(log, "snapshot -i"); click < 0 || snap < click {
		t.Errorf("no snapshot after the click:\n%s", log)
	}
	os.Remove(filepath.Join(rig.dir, "log"))
	for _, in := range []map[string]any{
		{"action": "fill", "ref": "e2", "text": "Ada"},
		{"action": "mouse", "mouse": "down"},
	} {
		if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, in); isErr || strings.Contains(text, untrustedWeb) {
			t.Errorf("%v: %v %s", in, isErr, text)
		}
	}
	if strings.Contains(rig.log(t), "snapshot") {
		t.Errorf("fill or mouse down took a snapshot:\n%s", rig.log(t))
	}
}

func TestABigPageIsCutShortAfterAnActionAndSavedWhole(t *testing.T) {
	rig := browserRig(t)
	var page strings.Builder
	for i := range 400 {
		fmt.Fprintf(&page, "- button \"Sunday, November %d, 2026 , 1906 US dollars\" [ref=e%d]\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(rig.dir, "page"), []byte(page.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "click", "ref": "e1"})
	if isErr || len(text) > browserAutoSnapChars+1_000 || !strings.Contains(text, "[ref=e0]") || strings.Contains(text, "[ref=e399]") {
		t.Fatalf("click returned %d chars, isErr %v:\n%.300s", len(text), isErr, text)
	}
	saved := regexp.MustCompile(`the whole page is in (\S+), for grep`).FindStringSubmatch(text)
	if saved == nil {
		t.Fatalf("no saved file named:\n%s", text[len(text)-300:])
	}
	if data, err := os.ReadFile(saved[1]); err != nil || string(data) != strings.TrimSpace(page.String()) {
		t.Errorf("saved file holds %d chars, want the whole page; %v", len(data), err)
	}
	text, isErr = browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "snapshot"})
	if isErr || !strings.Contains(text, "[ref=e399]") || strings.Contains(text, "lines shown") {
		t.Errorf("an asked-for snapshot under its cap was cut: %v\n%.300s", isErr, text)
	}
}

func TestWaitDoesNotSnapshot(t *testing.T) {
	rig := browserRig(t)
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "wait", "ms": 1}); isErr || strings.Contains(text, untrustedWeb) {
		t.Errorf("wait: %v %s", isErr, text)
	}
	if strings.Contains(rig.log(t), "snapshot") {
		t.Errorf("wait took a snapshot:\n%s", rig.log(t))
	}
}

func TestTheModelCannotRunJavaScriptInThePage(t *testing.T) {
	rig := browserRig(t)
	text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "eval", "js": "fetch('/api', {method: 'POST'})"})
	if !isErr || !strings.Contains(text, "action must be one of") {
		t.Errorf("eval: %v %s", isErr, text)
	}
	if strings.Contains(rig.log(t), "eval") {
		t.Errorf("the CLI was asked to eval:\n%s", rig.log(t))
	}
}

func TestRequestsListsWhatThePageSentWithItsBody(t *testing.T) {
	rig := browserRig(t)
	// This one execs the script, so argv, the --session prefix and stdout cross a real process.
	browserRun = (*exec.Cmd).Run
	text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "requests", "filter": "/api/save", "method": "post"})
	want := "POST http://localhost:3000/api/save 201 (Fetch)\n  body: [{\"firstName\":\"Ada\"}]\nGET http://localhost:3000/api/save?x pending (Fetch)"
	if isErr || !strings.Contains(text, "<"+untrustedWeb+">\n"+want+"\n</") {
		t.Errorf("requests: %v %s", isErr, text)
	}
	if !strings.Contains(rig.log(t), "--json network requests --filter /api/save --method POST") {
		t.Errorf("log:\n%s", rig.log(t))
	}
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "requests"}); !isErr || !strings.Contains(text, "needs filter") {
		t.Errorf("unfiltered requests: %v %s", isErr, text)
	}
}

func TestReadNeverFetchesAURLOfItsOwn(t *testing.T) {
	rig := browserRig(t)
	text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "read", "url": "https://evil.test/"})
	if isErr || !strings.Contains(text, "<"+untrustedWeb+">\n# Clients\nAda assigned PHQ-9") {
		t.Errorf("read: %v %s", isErr, text)
	}
	if strings.Contains(rig.log(t), "evil.test") {
		t.Errorf("read was handed the URL:\n%s", rig.log(t))
	}
}

func TestScrollMovesThePageOrBringsARefIntoViewAndSnapshots(t *testing.T) {
	rig := browserRig(t)
	for _, in := range []map[string]any{
		{"action": "scroll"},
		{"action": "scroll", "direction": "up", "px": 800},
		{"action": "scroll", "ref": "e2"},
	} {
		if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, in); isErr || !strings.Contains(text, "[ref=e1]") {
			t.Errorf("%v: %v %s", in, isErr, text)
		}
	}
	log := rig.log(t)
	for _, want := range []string{"scroll down 300", "scroll up 800", "scrollintoview @e2"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in log:\n%s", want, log)
		}
	}
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "scroll", "direction": "sideways"}); !isErr {
		t.Errorf("sideways scroll: %s", text)
	}
}

func TestWithoutABinaryTheBrowserIsNotOfferedAndSaysHowToConfigureIt(t *testing.T) {
	writeWebSettings(t, `{"web": {}}`)
	freshBrowserState(t)
	if tools := BrowserTools(); len(tools) != 0 {
		t.Errorf("offered %d browser tools with no binary", len(tools))
	}
	text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "open", "url": "http://localhost/"})
	if !isErr || !strings.Contains(text, `{"browser": {"binary": "/abs/path/to/agent-browser"}}`) {
		t.Errorf("no binary: %v %s", isErr, text)
	}
	writeWebSettings(t, `{"browser": {"binary": "agent-browser"}}`)
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "snapshot"}); !isErr || !strings.Contains(text, "absolute") {
		t.Errorf("relative binary: %v %s", isErr, text)
	}
}

func TestOnlyBlankAndTmpFilesAreOpenedOutsideTheWeb(t *testing.T) {
	for raw, allowed := range map[string]bool{
		"about:blank":               true,
		"file:///tmp/x.html":        true,
		"file:///private/tmp/x":     true,
		"file:///etc/passwd":        false,
		"file:///tmp/../etc/passwd": false,
		"javascript:alert(1)":       false,
		"data:text/html,hi":         false,
		"chrome://settings":         false,
	} {
		host, err := checkURL(raw)
		if (err == nil && host == "") != allowed {
			t.Errorf("checkURL(%q) = %q, %v; want allowed %v", raw, host, err, allowed)
		}
	}
}

func TestUploadHandsThePageOnlyFilesFromTheTempOrWorkingDirectory(t *testing.T) {
	rig := browserRig(t)
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "upload", "ref": "e2", "files": []string{"/etc/hosts"}}); !isErr || !strings.Contains(text, "working directory or the temp directory") {
		t.Errorf("upload /etc/hosts: %v %s", isErr, text)
	}
	if strings.Contains(rig.log(t), "upload") {
		t.Fatalf("the CLI was handed a file from outside:\n%s", rig.log(t))
	}

	fixture := filepath.Join(t.TempDir(), "export.zip")
	if err := os.WriteFile(fixture, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(fixture)
	here, _ := filepath.EvalSymlinks("browser.go")
	here, _ = filepath.Abs(here)
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "upload", "ref": "e2", "files": []string{fixture, "browser.go"}}); isErr {
		t.Fatalf("upload: %s", text)
	}
	if want := "upload @e2 " + resolved + " " + here; !strings.Contains(rig.log(t), want) {
		t.Errorf("log lacks %q:\n%s", want, rig.log(t))
	}

	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "upload", "ref": "e2", "name": "clients.csv", "text": "name\nAda\n"}); isErr {
		t.Fatalf("upload by name: %s", text)
	}
	lines := strings.Split(strings.TrimSpace(rig.log(t)), "\n")
	var made string
	for _, line := range lines {
		if fields := strings.Fields(line); len(fields) == 5 && fields[2] == "upload" {
			made = fields[4]
		}
	}
	if data, err := os.ReadFile(made); filepath.Base(made) != "clients.csv" || err != nil || string(data) != "name\nAda\n" {
		t.Errorf("uploaded %q holding %q, %v", made, data, err)
	}
	for _, name := range []string{"../x.csv", ".."} {
		if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "upload", "ref": "e2", "name": name, "text": "x"}); !isErr {
			t.Errorf("upload named %q: %s", name, text)
		}
	}
}

func TestMouseMoveSendsWholePixels(t *testing.T) {
	rig := browserRig(t)
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "mouse", "mouse": "move", "x": 10.4, "y": 19.6}); isErr {
		t.Fatalf("mouse move: %s", text)
	}
	if !strings.Contains(rig.log(t), "mouse move 10 20") {
		t.Errorf("log:\n%s", rig.log(t))
	}
}

func TestTheViewportIsSetBeforeOpeningAndRecording(t *testing.T) {
	rig := browserRig(t)
	for _, in := range []map[string]any{
		{"action": "open", "url": "http://localhost:3000/"},
		{"action": "click", "ref": "e1"},
		{"action": "record_start"},
	} {
		if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, in); isErr {
			t.Fatalf("%s: %s", in["action"], text)
		}
	}
	var steps []string
	for _, line := range strings.Split(strings.TrimSpace(rig.log(t)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "--session" {
			continue
		}
		switch fields[2] {
		case "set", "open", "click", "record":
			steps = append(steps, strings.Join(fields[2:min(len(fields), 4)], " "))
		}
	}
	want := []string{"set viewport", "open http://localhost:3000/", "click @e1", "set viewport", "record restart"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Errorf("steps = %q, want %q", steps, want)
	}
}

func TestOpenSetsTheRequestedViewportBeforeOpeningAndDefaultsWithoutOne(t *testing.T) {
	rig := browserRig(t)
	for _, in := range []map[string]any{
		{"action": "open", "url": "http://localhost:3000/", "width": 390, "height": 844},
		{"action": "open", "url": "http://localhost:3000/"},
	} {
		if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, in); isErr {
			t.Fatalf("open: %s", text)
		}
	}
	var steps []string
	for _, line := range strings.Split(strings.TrimSpace(rig.log(t)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "--session" && (fields[2] == "set" || fields[2] == "open") {
			steps = append(steps, strings.Join(fields[2:], " "))
		}
	}
	want := []string{"set viewport 390 844", "open http://localhost:3000/", "set viewport 1280 800", "open http://localhost:3000/"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Errorf("steps = %q, want %q", steps, want)
	}
}

func TestAHalfGivenOrNonPositiveViewportIsRefusedBeforeAnythingRuns(t *testing.T) {
	rig := browserRig(t)
	for _, in := range []map[string]any{
		{"action": "open", "url": "http://localhost:3000/", "width": 390},
		{"action": "record_start", "height": 844},
		{"action": "open", "url": "http://localhost:3000/", "width": 0, "height": 844},
		{"action": "record_start", "width": 390, "height": -1},
	} {
		text, isErr := browse(t, BrowserHandlers(), Callbacks{}, in)
		if !isErr || !strings.Contains(text, "width and height") {
			t.Errorf("%v: isErr %v, %q", in, isErr, text)
		}
	}
	if log := rig.log(t); log != "" {
		t.Errorf("a refused size still ran:\n%s", log)
	}
}

func TestARecordingScrollsToTheTargetAndMovesThePointerOntoItBeforeClicking(t *testing.T) {
	rig := browserRig(t)
	browserGlide = 0
	t.Cleanup(func() { browserGlide = 400 * time.Millisecond })
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "click", "ref": "e1"}); isErr {
		t.Fatalf("click: %s", text)
	}
	if log := rig.log(t); strings.Contains(log, "scrollintoview") || strings.Contains(log, "eval") {
		t.Errorf("a click with no recording was guided:\n%s", log)
	}
	os.Remove(filepath.Join(rig.dir, "log"))
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "record_start"}); isErr {
		t.Fatalf("record_start: %s", text)
	}
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "click", "ref": "e1"}); isErr {
		t.Fatalf("click: %s", text)
	}
	var steps []string
	for _, line := range strings.Split(strings.TrimSpace(rig.log(t)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "--session" {
			continue
		}
		switch fields[2] {
		case "eval", "scrollintoview", "mouse", "click", "--json":
			steps = append(steps, strings.Join(fields[2:min(len(fields), 6)], " "))
		}
	}
	want := []string{"eval (() => {", "scrollintoview @e1", "--json get box @e1", "mouse move 25 25", "click @e1"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Errorf("steps = %q, want %q", steps, want)
	}
}

func TestRecordStopWithAPathStopsOnlyThatRecording(t *testing.T) {
	rig := browserRig(t)
	t.Setenv("PATH", t.TempDir())
	start := func() string {
		t.Helper()
		text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "record_start"})
		going, found := strings.CutPrefix(strings.TrimSuffix(text, ". Stop it with record_stop."), "Recording to ")
		if isErr || !found {
			t.Fatalf("record_start: %s", text)
		}
		return going
	}
	stop := func(path string) (string, bool) {
		t.Helper()
		in := map[string]any{"action": "record_stop"}
		if path != "" {
			in["path"] = path
		}
		return browse(t, BrowserHandlers(), Callbacks{}, in)
	}

	going := start()
	if !strings.HasSuffix(going, ".mp4") || !strings.Contains(rig.log(t), "record restart "+rawRecording(going)) {
		t.Errorf("record_start must name an MP4 and have the CLI write the raw WebM beside it: %s\n%s", going, rig.log(t))
	}
	other := filepath.Join(filepath.Dir(going), "other.mp4")
	if text, isErr := stop(other); !isErr || text != "the recording going is "+going+", not "+other {
		t.Errorf("stopping another file: %v %s", isErr, text)
	}
	if strings.Contains(rig.log(t), "record stop") {
		t.Errorf("a mismatched path stopped the recording:\n%s", rig.log(t))
	}
	if text, isErr := stop(filepath.Dir(going) + "/./" + filepath.Base(going)); isErr {
		t.Errorf("stopping the recording going: %v %s", isErr, text)
	}
	if text, isErr := stop(going); !isErr || text != "nothing is recording, so there is no "+going+" to stop" {
		t.Errorf("stopping with nothing recording: %v %s", isErr, text)
	}

	going = start()
	if text, isErr := stop(""); isErr {
		t.Errorf("stopping without a path: %v %s", isErr, text)
	}
	dir, err := os.MkdirTemp("/tmp", "alux-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "record_start", "path": filepath.Join(dir, "clip.webm")}); !isErr ||
		!strings.Contains(text, "must end in .mp4") {
		t.Errorf("record_start to a .webm: %v %s", isErr, text)
	}
	asked := filepath.Join(dir, "clip.mp4")
	if text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "record_start", "path": asked}); isErr {
		t.Fatalf("record_start: %s", text)
	}
	if text, isErr := stop(asked); isErr {
		t.Errorf("the path record_start was given, unresolved, must stop it: %s", text)
	}
	if n := strings.Count(rig.log(t), "record stop"); n != 3 {
		t.Errorf("record stop ran %d times, want 3:\n%s", n, rig.log(t))
	}
}

func TestExportingSavesAnMP4WithStillStretchesCutToASecond(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	clip := func(t *testing.T, raw string, inputs ...string) {
		t.Helper()
		args := []string{"-v", "error"}
		for _, in := range inputs {
			args = append(args, "-f", "lavfi", "-i", in)
		}
		args = append(args, "-filter_complex", fmt.Sprintf("concat=n=%d", len(inputs)), "-c:v", "libvpx", raw)
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot make a test clip: %s", out)
		}
	}
	codec := func(t *testing.T, path string) string {
		t.Helper()
		out, err := exec.Command(filepath.Join(filepath.Dir(ffmpeg), "ffprobe"), "-v", "error", "-select_streams", "v:0",
			"-show_entries", "stream=codec_name:format=format_name", "-of", "default=nw=1:nk=1", path).Output()
		if err != nil {
			t.Fatalf("%s is not a video: %v", path, err)
		}
		return strings.Join(strings.Fields(string(out)), " ")
	}

	t.Run("still stretches are cut and the uncut one kept", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "clip.mp4")
		// Two seconds of motion, six still, one of motion.
		clip(t, rawRecording(path), "testsrc=d=2:s=64x48:r=10", "color=blue:d=6:s=64x48:r=10", "testsrc=d=1:s=64x48:r=10")
		text, err := exportVideo(context.Background(), ffmpeg, rawRecording(path), path)
		if err != nil {
			t.Fatal(err)
		}
		full := filepath.Join(dir, "clip-full.mp4")
		// The encoder refines a new still frame for about 0.7 s, which reads as motion, so 4.3 s of the 6 is cut.
		if !strings.HasPrefix(text, "Saved "+path+", with 4 s") || !strings.HasSuffix(text, "the uncut recording is "+full+".") {
			t.Errorf("text = %q", text)
		}
		for _, p := range []string{path, full} {
			if got := codec(t, p); !strings.HasPrefix(got, "h264 ") || !strings.Contains(got, "mp4") {
				t.Errorf("%s is %q, want h264 in mp4", p, got)
			}
		}
		if seconds := videoSeconds(context.Background(), ffmpeg, path); seconds < 4.2 || seconds > 5.2 {
			t.Errorf("condensed clip is %.1f s, want about 4.7", seconds)
		}
		if seconds := videoSeconds(context.Background(), ffmpeg, full); seconds < 8.5 || seconds > 9.5 {
			t.Errorf("uncut clip is %.1f s, want about 9", seconds)
		}
		if _, err := os.Stat(rawRecording(path)); !os.IsNotExist(err) {
			t.Errorf("the raw WebM was left behind: %v", err)
		}
	})

	t.Run("a small mark moving on a large page is not cut", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "clip.mp4")
		pointer := "color=white:s=1280x800:d=6:r=10[bg];color=black:s=28x28:d=6:r=10[m];[bg][m]overlay=x='20+t*30':y=20,format=yuv420p[out0]"
		clip(t, rawRecording(path), pointer, "testsrc=d=1:s=1280x800:r=10")
		text, err := exportVideo(context.Background(), ffmpeg, rawRecording(path), path)
		if err != nil || text != "Saved "+path+"." {
			t.Fatalf("text %q, err %v", text, err)
		}
	})

	t.Run("a clip with nothing to cut is only converted", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "clip.mp4")
		clip(t, rawRecording(path), "testsrc=d=3:s=63x47:r=10")
		text, err := exportVideo(context.Background(), ffmpeg, rawRecording(path), path)
		if err != nil || text != "Saved "+path+"." {
			t.Fatalf("text %q, err %v", text, err)
		}
		if got := codec(t, path); !strings.HasPrefix(got, "h264 ") {
			t.Errorf("%s is %q, want h264", path, got)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Errorf("want only clip.mp4 left, got %v", entries)
		}
	})
}

func TestTheBrowserWritesOnlyUnderTheTempDirectory(t *testing.T) {
	rig := browserRig(t)
	text, isErr := browse(t, BrowserHandlers(), Callbacks{}, map[string]any{"action": "screenshot", "path": "/etc/shot.png"})
	if !isErr || !strings.Contains(text, "under /tmp") {
		t.Errorf("screenshot into /etc: %v %s", isErr, text)
	}
	if strings.Contains(rig.log(t), "screenshot") {
		t.Errorf("the CLI was asked to write outside the temp directory:\n%s", rig.log(t))
	}
}
