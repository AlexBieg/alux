package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestReadShapesBecomeTheSameToolCalls(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ten.txt"), []byte(strings.Repeat("x\n", 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	p := func(name string) string { return filepath.Join(dir, name) }
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"sed -n 600,750p internal/agent/agent.go",
			[]string{`str_replace_based_edit_tool {"command":"view","path":"` + p("internal/agent/agent.go") + `","view_range":[600,750]}`}},
		{"sed -n '5p' a.go", []string{`str_replace_based_edit_tool {"command":"view","path":"` + p("a.go") + `","view_range":[5,5]}`}},
		{"cat a.go b.go", []string{
			`str_replace_based_edit_tool {"command":"view","path":"` + p("a.go") + `"}`,
			`str_replace_based_edit_tool {"command":"view","path":"` + p("b.go") + `"}`}},
		{"head -n 20 a.go", []string{`str_replace_based_edit_tool {"command":"view","path":"` + p("a.go") + `","view_range":[1,20]}`}},
		{"head -5 a.go", []string{`str_replace_based_edit_tool {"command":"view","path":"` + p("a.go") + `","view_range":[1,5]}`}},
		{"tail -n 3 ten.txt", []string{`str_replace_based_edit_tool {"command":"view","path":"` + p("ten.txt") + `","view_range":[8,-1]}`}},
		{"tail -n +4 a.go", []string{`str_replace_based_edit_tool {"command":"view","path":"` + p("a.go") + `","view_range":[4,-1]}`}},
		{"ls", []string{`str_replace_based_edit_tool {"command":"view","path":"` + dir + `"}`}},
		{"cd sub && sed -n 1,9p x.go; echo ----; ls lib", []string{
			`str_replace_based_edit_tool {"command":"view","path":"` + p("sub/x.go") + `","view_range":[1,9]}`,
			`str_replace_based_edit_tool {"command":"view","path":"` + p("sub/lib") + `"}`}},
		{`grep -rn "func screen\|func gate" internal/agent`,
			[]string{`grep {"path":"` + p("internal/agent") + `","pattern":"func screen\\|func gate"}`}},
		{`grep -n -A20 -i 'Foo' a.go | head -40`,
			[]string{`grep {"-A":20,"-i":true,"head_limit":40,"path":"` + p("a.go") + `","pattern":"Foo"}`}},
		{`grep -rl TODO --include='*.go'`, []string{`grep {"glob":"*.go","output_mode":"files_with_matches","path":"` + dir + `","pattern":"TODO"}`}},
		{`grep -c x a.go b.go`, []string{
			`grep {"output_mode":"count","path":"` + p("a.go") + `","pattern":"x"}`,
			`grep {"output_mode":"count","path":"` + p("b.go") + `","pattern":"x"}`}},
		{`rg -t go -C 3 -U 'a.*b' src`, []string{`grep {"-C":3,"multiline":true,"path":"` + p("src") + `","pattern":"a.*b","type":"go"}`}},
	} {
		var got []string
		for _, c := range readShape(tc.command, dir) {
			in, _ := json.Marshal(c.input)
			got = append(got, c.tool+" "+string(in))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%q\n got %q\nwant %q", tc.command, got, tc.want)
		}
	}
}

func TestAnythingButAPlainReadIsLeftToTheShell(t *testing.T) {
	for _, command := range []string{
		"echo ----",
		"cat a.go | wc -l",
		"cat a.go > b.go",
		"sed -n 1,5p $F",
		"sed -i 's/a/b/' a.go",
		"sed -n '/^func/p' a.go",
		"cat internal/*.go",
		"ls -la",
		"head -n 5 a.go b.go",
		"grep -v x a.go",
		"grep -w x a.go",
		"grep -F x a.go",
		"grep x a.go | sort",
		"go test ./... | grep FAIL",
		"cat a.go; rm a.go",
		"cat a.go &",
		"cat a.go || true",
		"cat \"$(pwd)/a.go\"",
		"cat a.go 2>&1",
		"jj log",
		"'cat' a.go",
	} {
		if calls := readShape(command, "/repo"); calls != nil {
			t.Errorf("%q matched as a read: %v", command, calls)
		}
	}
}

// Sandboxed reads used to run unasked; guardian never saw them, so the rule has to hold them on its own.
func TestASandboxedReadIsHeldWithoutAskingGuardian(t *testing.T) {
	a, f, r := sandboxedAgent(t, shellTurn("bash", "sed -n 3,9p a.go"), mockapi.Turn{Text: "done"})
	out := lastResult(t, a, &recorder{})
	if _, ran := r["t1"]; ran || !strings.HasPrefix(out, "Not run: it only reads files") ||
		!strings.Contains(out, `"view_range":[3,9]`) || !strings.Contains(out, `id "s1"`) {
		t.Errorf("the read ran or the result does not name the view call and id: ran %v, %q", r, out)
	}
	if len(f.menus) != 0 || len(f.asked) != 0 {
		t.Errorf("guardian was asked: menus %v, screened %v", f.menus, f.asked)
	}
	if !slices.Equal(f.records, []string{"suggested/ignored sed -n 3,9p a.go"}) {
		t.Errorf("recorded %v, want the rule's suggestion logged as ignored", f.records)
	}
}

func TestAReadIsAcceptedOnlyByAViewOfTheSameFile(t *testing.T) {
	dir := t.TempDir()
	target, other := filepath.Join(dir, "a.go"), filepath.Join(dir, "b.go")
	for _, p := range []string{target, other} {
		if err := os.WriteFile(p, []byte("package a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	viewTurn := func(id, path string) mockapi.Turn {
		return mockapi.Turn{Tools: []mockapi.ToolUse{{ID: id, Name: editorTool, Input: map[string]any{"command": "view", "path": path}}}}
	}
	a, f, _ := sandboxedAgent(t, shellTurn("bash", "cat "+target), viewTurn("t2", other), viewTurn("t3", target), mockapi.Turn{Text: "done"})
	a.Handlers[editorTool] = BuiltinHandlers()[editorTool]
	var accepted []string
	cb := (&recorder{}).callbacks()
	cb.OnCall = func(c Call, _ anthropic.ContentBlockParamUnion) {
		if c.ID == "t2" {
			accepted = slices.Clone(f.records)
		}
	}
	a.RunTurn(context.Background(), user("go"), cb)
	if len(accepted) != 0 {
		t.Errorf("a view of another file was taken as accepting: %v", accepted)
	}
	if !slices.Equal(f.records, []string{"suggested/accepted cat " + target}) {
		t.Errorf("recorded %v, want accepted by the view of %s", f.records, target)
	}
}

func TestAReadRunsWhenTheAgentHasNoTool(t *testing.T) {
	a, _, r := sandboxedAgent(t, shellTurn("bash", "cat a.go"), mockapi.Turn{Text: "done"})
	a.Tools = func() []anthropic.ToolUnionParam { return []anthropic.ToolUnionParam{{OfTool: BashTool()}} }
	lastResult(t, a, &recorder{})
	if r["t1"] != "sentinel" {
		t.Errorf("an agent with no editor had its read held: ran %v", r)
	}
}

func TestADeclinedReadIsPutToThePersonAndRuns(t *testing.T) {
	a, _, r := sandboxedAgent(t, shellTurn("bash", "cat a.go"), declineTurn("s1", "need raw bytes"), mockapi.Turn{Text: "done"})
	cb := (&recorder{}).callbacks()
	var reasons []string
	cb.Require = func(_ context.Context, _, _, reason string) bool { reasons = append(reasons, reason); return true }
	a.RunTurn(context.Background(), user("go"), cb)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "need raw bytes") || r["t2"] != "sentinel" {
		t.Errorf("asked %q, ran %v; want one prompt with the reason, then a sandboxed run", reasons, r)
	}
}
