package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/alexbieg/alux/internal/llm"
	"github.com/alexbieg/alux/internal/records"
	"github.com/alexbieg/alux/internal/vault"
	"github.com/anthropics/anthropic-sdk-go"
)

var vaultRoot = vault.Root

const migrationToolDescription = `Track moving the markdown vault into the record store, one note at a time, so several agents can work without doing a note twice. Follow the migration procedure stored in the record store (search procedures for "migrate one Obsidian vault note").

op "seed": list every .md note in the vault into the ledger as pending. Safe to repeat; it adds new notes and reports ones that have vanished.
op "claim": take the next pending note, or a named one with path, and get a token. A claim left unfinished for an hour can be taken by someone else.
op "finish": close a note with the token from claim. status done needs the ids of the records it became; skipped and failed need a reason in note; pending hands it back. A note never claimed (an older note found while following a supersession chain, say) can be finished without a token.
op "status": counts by status, open claims, and failures. The migration is complete when nothing is pending or in progress.`

func MigrationTools() []anthropic.ToolUnionParam {
	if records.Path() == "" || vaultRoot() == "" {
		return nil
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name:        "vault_migration",
		Description: anthropic.String(migrationToolDescription),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"op":      map[string]any{"type": "string", "enum": []string{"seed", "claim", "finish", "status"}},
				"path":    str("claim, finish: the note's path relative to the vault root, e.g. Decisions/Foo.md."),
				"token":   str("finish: the token claim returned."),
				"status":  map[string]any{"type": "string", "enum": []string{"done", "skipped", "failed", "pending"}, "description": "finish: how the note ended."},
				"records": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "finish: ids of the records the note became."},
				"note":    str("finish: why it was skipped or failed, or anything worth knowing about it."),
			},
			Required:    []string{"op"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}}
}

func MigrationHandlers() map[string]Handler { return MigrationHandlersAs("main", llm.Model) }

// MigrationHandlersAs puts the named agent and its model on every claim it takes.
func MigrationHandlersAs(agentName string, model func() string) map[string]Handler {
	if records.Path() == "" || vaultRoot() == "" {
		return nil
	}
	return map[string]Handler{"vault_migration": func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
		return migrationAs(ctx, call, cb, agentName+" pid "+strconv.Itoa(os.Getpid())+" on "+model())
	}}
}

func runMigration(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	return migrationAs(ctx, call, cb, "main pid "+strconv.Itoa(os.Getpid())+" on "+llm.Model())
}

// Local bookkeeping with every change guarded by a claim token, so no approval prompt.
func migrationAs(_ context.Context, call Call, cb Callbacks, claimant string) anthropic.ContentBlockParamUnion {
	var in struct {
		Op      string   `json:"op"`
		Path    string   `json:"path"`
		Token   string   `json:"token"`
		Status  string   `json:"status"`
		Records []string `json:"records"`
		Note    string   `json:"note"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	label := strings.Join(slices.DeleteFunc([]string{"migration: " + in.Op, in.Path, in.Status}, func(s string) bool { return s == "" }), " · ")
	cb.toolStart(call.ID, label, KindVault)
	output, err := migrate(claimant, in.Op, in.Path, in.Token, in.Status, in.Records, in.Note)
	if err != nil {
		cb.toolResult(call.ID, label, err.Error(), KindVault)
		return failed(call.ID, err)
	}
	cb.toolResult(call.ID, label, output, KindVault)
	return ok(call.ID, output)
}

func migrate(claimant, op, path, token, status string, ids []string, note string) (string, error) {
	store, err := openRecordStore()
	if err != nil {
		return "", fmt.Errorf("record store unavailable: %w", err)
	}
	switch op {
	case "seed":
		paths, err := vaultNotes(vaultRoot())
		if err != nil {
			return "", err
		}
		added, gone, err := store.SeedMigration(paths)
		if err != nil {
			return "", err
		}
		out := fmt.Sprintf("%d notes in the vault, %d newly added to the ledger.", len(paths), added)
		if len(gone) > 0 {
			out += fmt.Sprintf("\n%d ledger notes no longer exist in the vault:\n  %s", len(gone), strings.Join(gone, "\n  "))
		}
		return out, nil
	case "claim":
		c, err := store.Claim(path, claimant)
		if errors.Is(err, records.ErrNothingToClaim) {
			return err.Error() + ". Check op status.", nil
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Claimed %s\nclaim: %s\nRead it with vault_read, migrate it, then pass this claim id as token to finish.", c.Path, c.Token), nil
	case "finish":
		if err := store.Finish(path, token, status, ids, note); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s marked %s.", path, status), nil
	case "status":
		st, err := store.MigrationStatus()
		if err != nil {
			return "", err
		}
		if st.Total == 0 {
			return "The ledger is empty: run op seed first.", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d notes:", st.Total)
		for _, s := range []string{"pending", "in_progress", "done", "skipped", "failed"} {
			fmt.Fprintf(&b, " %s %d,", s, st.Counts[s])
		}
		out := strings.TrimSuffix(b.String(), ",") + "\n"
		if st.Counts["pending"]+st.Counts["in_progress"] == 0 {
			out += "Complete: nothing is pending or in progress.\n"
		}
		for _, c := range st.InProgress {
			expired := ""
			if c.Expired {
				expired = ", expired"
			}
			out += fmt.Sprintf("claimed: %s by %s at %s%s\n", c.Path, c.By, c.At, expired)
		}
		for _, f := range st.Failed {
			out += fmt.Sprintf("failed: %s: %s\n", f.Path, f.Note)
		}
		return strings.TrimRight(out, "\n"), nil
	}
	return "", fmt.Errorf("unknown op %q: use seed, claim, finish or status", op)
}

// Hidden directories (.obsidian, .trash) are Obsidian's own, not notes.
func vaultNotes(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	return paths, err
}
