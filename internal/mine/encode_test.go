package mine

import (
	"encoding/json"
	"slices"
	"testing"
)

func bashInput(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func TestABashCallBecomesOneSymbolPerCommandItRuns(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"go build ./... && go vet ./... && go test ./...", []string{"go_build", "go_vet", "go_test"}},
		{"go test ./... 2>&1 | tail -20", []string{"go_test"}},
		{"jj log >/dev/null; jj commit -m 'x && y'", []string{"jj_log", "jj_commit"}},
		{`PATH=$(echo "$PATH" | tr : '\n') jj commit -m done`, []string{"jj_commit"}},
		{"cd /tmp && ls && cat a.txt && rm a.txt", []string{"rm"}},
		{"for f in *.go; do gofmt -l $f; done", []string{"gofmt"}},
		{"python3 - <<'EOF'\nimport os\nrm = 1\nEOF\njj diff", []string{"python", "jj_diff"}},
		{"x=$(jj log -r @ --no-graph) || make check", []string{"make_check"}},
		{"/usr/local/bin/gh pr view 12", []string{"gh_pr"}},
		{"echo '--- done'", nil},
	}
	for _, c := range cases {
		if got := Encode("bash", bashInput(c.cmd)); !slices.Equal(got, c.want) {
			t.Errorf("Encode(bash %q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestOnlyCreatingAFileIsKeptFromTheEditTool(t *testing.T) {
	create, _ := json.Marshal(map[string]string{"command": "create", "path": "/secret/notes.md"})
	view, _ := json.Marshal(map[string]string{"command": "view", "path": "/a.go"})
	if got := Encode(editTool, create); !slices.Equal(got, []string{"create_file"}) {
		t.Errorf("create = %q, want [create_file]", got)
	}
	if got := Encode(editTool, view); got != nil {
		t.Errorf("view = %q, want nothing", got)
	}
	for _, name := range []string{"grep", "glob", "start_skill", "finish_skill"} {
		if got := Encode(name, nil); got != nil {
			t.Errorf("%s = %q, want nothing", name, got)
		}
	}
	if got := Encode("pr_detail", nil); !slices.Equal(got, []string{"pr_detail"}) {
		t.Errorf("pr_detail = %q", got)
	}
}
