package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/workers"
)

func TestAClaimedWorkerSurvivesNewSessionAndRidesOnTheNextPrompt(t *testing.T) {
	s, server := newTestSession(t, mockapi.Turn{Text: "heard"})
	claimed := false
	s.ClaimWorkers = func() []workers.Finished {
		if claimed {
			return nil
		}
		claimed = true
		return []workers.Finished{{Key: "ABC-1", Final: workers.Final{V: workers.Version, State: workers.Done, How: "finished", Message: "PR is up."}}}
	}
	s.collectWorkers()
	if out := transcript(s.Snapshot()); !strings.Contains(out, "worker ABC-1 finished: done") {
		t.Errorf("no info line for the claim:\n%s", out)
	}

	s.NewSession()
	s.Submit("hi")
	settled(t, s, "heard")
	raw, err := json.Marshal(server.LastRequest()["messages"])
	if err != nil {
		t.Fatal(err)
	}
	sent := string(raw)
	block, hi := strings.Index(sent, "Worker ABC-1 finished: done (finished)"), strings.LastIndex(sent, "hi")
	if block < 0 || !strings.Contains(sent, "PR is up.") || hi < block {
		t.Errorf("the worker block did not precede the prompt:\n%s", sent)
	}
}
