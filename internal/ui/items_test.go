package ui

import (
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/subagent"
)

// A call refused before it ran was recorded under its tool's name alone; its input still names the command.
func TestARefusedCallsBoxNamesItsCommand(t *testing.T) {
	items, _ := transcriptItems([]subagent.TranscriptEvent{
		{Kind: subagent.TranscriptTool, Name: "bash", Label: "bash", Input: []byte(`{"command":"cat go.mod"}`), Failed: true, Output: "Not run: it only reads files"},
	}, 0)
	if len(items) != 1 || items[0].Text != "$ cat go.mod (failed)" || items[0].Tool != agent.KindBash {
		t.Errorf("items = %+v, want one bash box headed $ cat go.mod (failed)", items)
	}
}
