package subagent

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestASubagentRecordWriteCarriesTheDispatchingSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	t.Setenv("ALUX_RECORDS", path)
	def := echo(false)
	def.Tools = func() []anthropic.ToolUnionParam { return agent.RecordTools() }
	def.Handlers = agent.RecordHandlersAs("curator", func() string { return "mock" })
	m, _ := manager(t, def,
		mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "vault_record", Input: map[string]any{
			"op": "entity", "entity": map[string]any{"type": "system", "name": "alux", "sensitivity": "normal"}}}}},
		mockapi.Turn{Text: "Filed."},
	)
	m.Session = "the-session"

	if _, err := m.Dispatch(context.Background(), "echo", "file it", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var agentName string
	var session sql.NullString
	if err := db.QueryRow(`SELECT agent, session FROM txns`).Scan(&agentName, &session); err != nil {
		t.Fatal(err)
	}
	if agentName != "curator" || session.String != "the-session" {
		t.Errorf("txn written by %q under session %q, want curator under the-session", agentName, session.String)
	}
}
