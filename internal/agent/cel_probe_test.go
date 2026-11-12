package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/records"
	"github.com/alexbieg/alux/internal/rulecel"
	"github.com/anthropics/anthropic-sdk-go"
)

// TestCelProbe replays a candidate condition over every recorded main-session call the rule's trigger matches,
// or with ALUX_CEL_TOOL a skill call rule's when over every recorded call to that tool.
// Calls carry no session repo offline, so a clause that needs the session's repo matches only calls with a path.
func TestCelProbe(t *testing.T) {
	if os.Getenv("ALUX_CEL_PROBE") == "" {
		t.Skip("set ALUX_CEL_PROBE=1, then ALUX_CEL_RULE to a rule id and ALUX_CEL_WHEN to a condition to replay a hold, optionally with ALUX_CEL_TRIGGER, " +
			"or ALUX_CEL_TOOL to a tool and ALUX_CEL_WHEN to a call rule's when to replay skill evidence, optionally with ALUX_CEL_INPUTS and ALUX_CEL_VARS as JSON")
	}
	if tool := os.Getenv("ALUX_CEL_TOOL"); tool != "" {
		probeEvidence(t, tool, os.Getenv("ALUX_CEL_WHEN"))
		return
	}
	id, when := os.Getenv("ALUX_CEL_RULE"), os.Getenv("ALUX_CEL_WHEN")
	if id == "" || when == "" {
		t.Fatal("set ALUX_CEL_RULE to a rule id and ALUX_CEL_WHEN to the candidate condition")
	}
	if _, err := rulecel.Compile(when); err != nil {
		t.Fatalf("the condition does not compile: %v", err)
	}
	// TestMain points HOME at a temp dir, so the real one comes from the user database.
	u, err := osuser.Current()
	if err != nil {
		t.Fatal(err)
	}
	store := realStoreCopy(t, u.HomeDir)
	rules, err := store.Rules()
	if err != nil {
		t.Fatal(err)
	}
	var rule records.Rule
	for _, r := range rules {
		if r.ID == id {
			rule = r
		}
	}
	if rule.ID == "" {
		t.Fatalf("no current rule %s", id)
	}
	rule.When = when
	if trigger := os.Getenv("ALUX_CEL_TRIGGER"); trigger != "" {
		// Into a fresh slice: decoding over the stored clauses would merge into them, keeping always: true.
		var clauses []records.Clause
		if err := json.Unmarshal([]byte(trigger), &clauses); err != nil {
			t.Fatalf("ALUX_CEL_TRIGGER is not a trigger list: %v", err)
		}
		rule.Trigger = clauses
	}
	r := NewRuleInjector("")
	r.userHome = u.HomeDir
	// The repo today says nothing about a recorded session's stack, so every revision has a descendant.
	r.descendants = func(_, _ string) ([]string, error) { return []string{"stub"}, nil }
	noop := func(_ context.Context, call Call, _ Callbacks) anthropic.ContentBlockParamUnion {
		return ok(call.ID, "")
	}
	a := &Agent{Handlers: map[string]Handler{}}

	files, err := filepath.Glob(filepath.Join(u.HomeDir, paths.State, "sessions", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var matched, hold, pass, cut int
	var errs, held []string
	for _, file := range files {
		session := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		r.Reset()
		var earlier []string
		for _, e := range toolEvents(t, file) {
			if !shellTool(e.Name) && e.Name != editorTool {
				a.Handlers[e.Name] = noop
			}
			call := Call{ID: "replay", Name: e.Name, Input: e.Input}
			resolved := a.ruleCall(t.Context(), call)
			if resolved.Path != "" {
				resolved.Repo, resolved.Path = r.locate(store, resolved.Path)
			}
			r.remember(earlier)
			earlier = resolved.Commands
			if !rule.Fires(resolved) {
				continue
			}
			matched++
			if len(e.Input) > 0 && e.Input[0] == '"' {
				cut++
				continue
			}
			holds, evidence, err := r.evalWhen(rule, resolved, e.Input, "")
			switch {
			case err != nil:
				errs = append(errs, fmt.Sprintf("%s %s: %v", session, e.Name, err))
			case holds:
				hold++
				line := fmt.Sprintf("%s %s %s", session, e.Name, e.Input)
				if len(evidence) > 0 {
					line += "\n  because " + strings.Join(evidence, "; ")
				}
				held = append(held, line)
			default:
				pass++
			}
		}
	}

	fmt.Printf("rule %s: %s: %s\nwhen %s\nsessions %d; descendants stubbed to one change\n\n", rule.ID, rule.Subject, rule.Title, when, len(files))
	fmt.Printf("%8s %11s %10s %7s %10s\n", "matched", "would hold", "would run", "errors", "input cut")
	fmt.Printf("%8d %11d %10d %7d %10d\n", matched, hold, pass, len(errs), cut)
	for _, e := range errs {
		fmt.Println("error", e)
	}
	for _, h := range held {
		fmt.Println("\nheld", h)
	}
}

// probeEvidence lists the recorded calls to tool a skill call rule's when would not count, since those break a run.
func probeEvidence(t *testing.T, tool, when string) {
	if when == "" {
		t.Fatal("set ALUX_CEL_WHEN to the candidate call rule condition")
	}
	ev, err := rulecel.CompileEvidence(when)
	if err != nil {
		t.Fatalf("the condition does not compile: %v", err)
	}
	var inputs map[string]any
	if s := os.Getenv("ALUX_CEL_INPUTS"); s != "" {
		if err := json.Unmarshal([]byte(s), &inputs); err != nil {
			t.Fatalf("ALUX_CEL_INPUTS is not a JSON object: %v", err)
		}
	}
	var vars map[string]string
	if s := os.Getenv("ALUX_CEL_VARS"); s != "" {
		if err := json.Unmarshal([]byte(s), &vars); err != nil {
			t.Fatalf("ALUX_CEL_VARS is not a JSON object of strings: %v", err)
		}
	}
	u, err := osuser.Current()
	if err != nil {
		t.Fatal(err)
	}
	noop := func(_ context.Context, call Call, _ Callbacks) anthropic.ContentBlockParamUnion {
		return ok(call.ID, "")
	}
	a := &Agent{Handlers: map[string]Handler{}}
	files, err := filepath.Glob(filepath.Join(u.HomeDir, paths.State, "sessions", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var with int
	var counted, missed, errs []string
	for _, file := range files {
		session := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		for _, e := range toolEvents(t, file) {
			if e.Name != tool {
				continue
			}
			with++
			line := fmt.Sprintf("%s %s %s", session, e.Name, e.Input)
			args := map[string]any{}
			if err := json.Unmarshal(e.Input, &args); err != nil {
				errs = append(errs, fmt.Sprintf("%s: the input is cut or not an object", line))
				continue
			}
			if !shellTool(e.Name) && e.Name != editorTool {
				a.Handlers[e.Name] = noop
			}
			resolved := a.ruleCall(t.Context(), Call{ID: "replay", Name: e.Name, Input: e.Input})
			holds, err := ev.Eval(rulecel.Call{Tool: e.Name, Args: args, Commands: resolved.Commands}, inputs, vars, nil)
			switch {
			case err != nil:
				errs = append(errs, fmt.Sprintf("%s: %v", line, err))
			case holds:
				counted = append(counted, line)
			default:
				missed = append(missed, line)
			}
		}
	}

	fmt.Printf("tool %s\nwhen %s\nsessions %d\n\n", tool, when, len(files))
	fmt.Printf("%13s %11s %9s %7s\n", "with the tool", "would count", "would not", "errors")
	fmt.Printf("%13d %11d %9d %7d\n", with, len(counted), len(missed), len(errs))
	for _, e := range errs {
		fmt.Println("error", e)
	}
	for _, m := range missed {
		fmt.Println("\nmissed", m)
	}
	if len(counted) <= 10 {
		for _, c := range counted {
			fmt.Println("\ncounted", c)
		}
	}
}

type recordedCall struct {
	Kind  string          `json:"kind"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// toolEvents is every tool call a session transcript recorded; an input over the transcript's cap is a JSON string.
func toolEvents(t *testing.T, path string) []recordedCall {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []recordedCall
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var e recordedCall
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Kind == "tool" && e.Name != "" {
			out = append(out, e)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

// realStoreCopy opens a copy of the user's record store, so the replay never writes to the real one.
func realStoreCopy(t *testing.T, home string) *records.Store {
	t.Helper()
	src := filepath.Join(home, paths.Share, "records.db")
	dir := t.TempDir()
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(src+suffix, filepath.Join(dir, "records.db"+suffix)); err != nil && !(suffix != "" && os.IsNotExist(err)) {
			t.Fatal(err)
		}
	}
	s, err := records.Open(filepath.Join(dir, "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
