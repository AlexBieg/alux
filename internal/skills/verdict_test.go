package skills

import (
	"fmt"
	"os"
	"testing"
)

const verdictDir = home + "/.local/state/alux/skills/dev-cycle/20261014T120000Z"

// verdict checks the built-in dev-verdict at round with the given findings and review files, after a diff and a findings edit.
func verdict(t *testing.T, round int, out map[string]any, files map[string]string) Result {
	t.Helper()
	data, err := Builtin("dev-verdict.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse("dev-verdict", data)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Bind(s, map[string]any{"id": "c1", "bookmark": "b1", "task": "do it", "round": round}, started, home)
	if err != nil {
		t.Fatal(err)
	}
	r.Out = out
	log := []Event{
		{Tool: "bash", Input: []byte(`{"command":"jj diff -r b1"}`), Commands: []string{"jj diff -r b1"}},
		call(editorTool, map[string]any{"command": "str_replace", "path": verdictDir + "/findings-c1.md"}),
	}
	env := Env{ReadFile: func(p string) ([]byte, error) {
		body, ok := files[p]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(body), nil
	}}
	return Check(r, log, env)
}

func review(round int) string { return fmt.Sprintf("%s/review-c1-%d.md", verdictDir, round) }

const findings = verdictDir + "/findings-c1.md"

func TestAReviewBlockingLineMissingFromTheFindingsLeavesTheVerdictUnmet(t *testing.T) {
	files := map[string]string{
		review(1): "[blocking] a.go:12: nil map -> make it\n[non-blocking] b.go:3: name -> rename\n",
		findings:  "## round 1\n[non-blocking] b.go:3: name -> rename\n",
	}
	if n := node(t, verdict(t, 1, nil, files), "verdict"); n.State != Pending {
		t.Fatalf("verdict with a.go:12 dropped from the findings = %s, want pending", n.State)
	}
	files[findings] = "## round 1\n- [blocking] a.go:12-14: nil map -> make it\n"
	if n := node(t, verdict(t, 1, nil, files), "verdict"); n.State != Done {
		t.Fatalf("verdict with a.go:12 carried over = %s %v, want done", n.State, n.Missing)
	}
}

func TestAnOverruleWithNoReasonDoesNotCoverABlockingLine(t *testing.T) {
	files := map[string]string{
		review(1): "[blocking] a.go:12: nil map -> make it\n",
		findings:  "## round 1\n[non-blocking] a.go:12: nil map -> make it (overruled: )\n",
	}
	if n := node(t, verdict(t, 1, nil, files), "verdict"); n.State != Pending {
		t.Fatalf("verdict with an empty overrule = %s, want pending", n.State)
	}
	files[findings] = "## round 1\n[non-blocking] a.go:12: nil map -> make it (overruled: the map is always set by New)\n"
	if n := node(t, verdict(t, 1, map[string]any{"kind": "clean"}, files), "verdict"); n.State != Done {
		t.Fatalf("verdict with a reasoned overrule = %s %v, want done", n.State, n.Missing)
	}
}

func TestCleanIsRefusedOnlyForABlockingLineInThisRoundsSection(t *testing.T) {
	files := map[string]string{
		review(1): "[blocking] a.go:12: nil map -> make it\n",
		review(2): "clean\n",
		findings:  "## round 1\n[blocking] a.go:12: nil map -> make it\n\n## round 2\n[non-blocking] b.go:3: name -> rename\n",
	}
	clean := map[string]any{"kind": "clean"}
	if res := verdict(t, 1, clean, files); res.Complete {
		t.Fatal("kind clean with a [blocking] line in round 1's section was complete")
	}
	if res := verdict(t, 1, map[string]any{"kind": "fix"}, files); !res.Complete {
		t.Fatalf("kind fix in round 1 was refused: %+v", res.Nodes)
	}
	if res := verdict(t, 2, clean, files); !res.Complete {
		t.Fatalf("kind clean in round 2, whose section has no [blocking] line, was refused: %+v", res.Nodes)
	}
}
