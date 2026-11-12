package session

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/classify"
	"github.com/alexbieg/alux/internal/mockapi"
)

// sidecar stands in for the Python classifier, answering every command with p.
func sidecar(t *testing.T, p string) *classify.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Write([]byte(`{"writes": ` + p + `}`))
	}))
	t.Cleanup(srv.Close)
	port, err := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatalf("parsing sidecar port: %v", err)
	}
	return classify.Attach(port, func(string) {})
}

// The verdict usually lands after the command has already answered, so it has to reach a box
// that is no longer running — the case resolveTool deliberately does not cover.
func TestAWriteVerdictReachesAFinishedBox(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "gofmt -w internal/agent/agent.go"}}}},
		mockapi.Turn{Text: "done"},
	)
	s.Classifier = sidecar(t, "0.97")
	s.Submit("go")
	settled(t, s, "done")

	waitUntil(t, "the write verdict", func(sn Snapshot) bool {
		for _, item := range sn.Items {
			if item.Kind == KindTool && item.Writes == classify.Writes {
				return true
			}
		}
		return false
	}, s)
}

func TestAReadVerdictLeavesTheBoxUntinted(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "grep -rn foo internal/"}}}},
		mockapi.Turn{Text: "done"},
	)
	s.Classifier = sidecar(t, "0.03")
	s.Submit("go")
	settled(t, s, "done")

	for _, item := range toolItems(s.Snapshot()) {
		if item.Writes == classify.Writes {
			t.Errorf("a read was tinted as a write: %q", item.Text)
		}
	}
}

// Without a classifier configured every box stays untinted, which is the shape alux runs
// in unless settings.json turns the feature on.
func TestNoClassifierLeavesEveryBoxUnknown(t *testing.T) {
	s, _ := newTestSession(t,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "bash", Input: map[string]any{"command": "rm -rf build/"}}}},
		mockapi.Turn{Text: "done"},
	)
	s.Submit("go")
	settled(t, s, "done")

	for _, item := range toolItems(s.Snapshot()) {
		if item.Writes != classify.Unknown {
			t.Errorf("box classified with no classifier wired: %q is %v", item.Text, item.Writes)
		}
	}
}

// A bash box that reports on a process rather than running a command has no command to read,
// and must not be sent to the model as one.
func TestProcessBoxesAreNotClassified(t *testing.T) {
	for _, label := range []string{"output of proc 3", "kill proc 3"} {
		if _, ok := classify.Command(label); ok {
			t.Errorf("%q was taken for a command", label)
		}
	}
}
