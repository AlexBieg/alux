package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/scrub"
)

func TestMigrationToolWalksTheVaultToCompletion(t *testing.T) {
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	root := t.TempDir()
	for _, p := range []string{"HOME.md", "Decisions/A.md", ".obsidian/workspace.md", "Systems/notes.txt"} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := vaultRoot
	vaultRoot = func() string { return root }
	t.Cleanup(func() { vaultRoot = orig })
	if MigrationHandlers()["vault_migration"] == nil {
		t.Fatal("vault_migration not registered with a vault and a record store")
	}

	call := func(h Handler, name string, input map[string]any) (string, bool) {
		raw, _ := json.Marshal(input)
		res := h(context.Background(), Call{ID: "c", Name: name, Input: raw}, Callbacks{})
		var text string
		for _, c := range res.OfToolResult.Content {
			if c.OfText != nil {
				text += c.OfText.Text
			}
		}
		return text, res.OfToolResult.IsError.Or(false)
	}
	mig := func(input map[string]any) string {
		out, isErr := call(runMigration, "vault_migration", input)
		if isErr {
			t.Fatalf("%v: %s", input, out)
		}
		return out
	}

	if out := mig(map[string]any{"op": "seed"}); !strings.HasPrefix(out, "2 notes in the vault, 2 newly added") {
		t.Fatalf("seed should list only the two notes outside hidden folders: %q", out)
	}

	call(runRecord, "vault_record", map[string]any{"op": "entity", "entity": map[string]any{"type": "system", "name": "alux", "sensitivity": "normal"}})
	stored, _ := call(runRecord, "vault_record", map[string]any{"op": "add", "kind": "claim", "subject": "alux", "title": "Migrated", "sensitivity": "normal",
		"source": map[string]any{"type": "file", "ref": "Decisions/A.md"}})
	id := regexp.MustCompile(`rec_[0-9a-f]+`).FindString(stored)

	token := regexp.MustCompile(`claim: (clm_[0-9a-f]+)`)
	claimed := scrub.Text(mig(map[string]any{"op": "claim"}))
	if !strings.Contains(claimed, "Claimed Decisions/A.md") || !token.MatchString(claimed) {
		t.Fatalf("the claim id must reach the agent through the secret scrubber: %q", claimed)
	}
	mig(map[string]any{"op": "finish", "path": "Decisions/A.md", "token": token.FindStringSubmatch(claimed)[1], "status": "done", "records": []string{id}})

	claimed = mig(map[string]any{"op": "claim"})
	mig(map[string]any{"op": "finish", "path": "HOME.md", "token": token.FindStringSubmatch(claimed)[1], "status": "skipped", "note": "index note, no durable facts"})

	if out := mig(map[string]any{"op": "status"}); !strings.Contains(out, "done 1") || !strings.Contains(out, "skipped 1") || !strings.Contains(out, "Complete") {
		t.Fatalf("status: %q", out)
	}
}

func TestRecordsAndClaimsNameTheAgentThatMadeThem(t *testing.T) {
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "A.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := vaultRoot
	vaultRoot = func() string { return root }
	t.Cleanup(func() { vaultRoot = orig })

	call := func(h Handler, name string, input map[string]any) string {
		raw, _ := json.Marshal(input)
		res := h(context.Background(), Call{ID: "c", Name: name, Input: raw}, Callbacks{})
		var text string
		for _, c := range res.OfToolResult.Content {
			if c.OfText != nil {
				text += c.OfText.Text
			}
		}
		if res.OfToolResult.IsError.Or(false) {
			t.Fatalf("%v: %s", input, text)
		}
		return text
	}
	sonnet := func() string { return "claude-sonnet-5-5" }
	record := RecordHandlersAs("migrator", sonnet)["vault_record"]
	mig := MigrationHandlersAs("migrator", sonnet)["vault_migration"]

	call(record, "vault_record", map[string]any{"op": "entity", "entity": map[string]any{"type": "system", "name": "alux", "sensitivity": "normal"}})
	stored := call(record, "vault_record", map[string]any{"op": "add", "kind": "claim", "subject": "alux", "title": "Migrated", "sensitivity": "normal",
		"source": map[string]any{"type": "file", "ref": "A.md"}})
	id := regexp.MustCompile(`rec_[0-9a-f]+`).FindString(stored)
	if got := call(runRecordRead, "vault_record_read", map[string]any{"op": "get", "id": id}); !strings.Contains(got, "by agent/migrator on claude-sonnet-5-5") {
		t.Fatalf("record should be attributed to the migrator: %q", got)
	}

	call(mig, "vault_migration", map[string]any{"op": "seed"})
	call(mig, "vault_migration", map[string]any{"op": "claim"})
	if got := call(mig, "vault_migration", map[string]any{"op": "status"}); !regexp.MustCompile(`claimed: A\.md by migrator pid \d+ on claude-sonnet-5-5`).MatchString(got) {
		t.Fatalf("claim should name the migrator: %q", got)
	}
}
