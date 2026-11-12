package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/records"
	"github.com/alexbieg/alux/internal/vault"
	"github.com/anthropics/anthropic-sdk-go"
)

// AutoMigrateOn reports whether settings.json sets "auto_migrate": true.
func AutoMigrateOn() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return false
	}
	var settings struct {
		AutoMigrate bool `json:"auto_migrate"`
	}
	return json.Unmarshal(data, &settings) == nil && settings.AutoMigrate
}

// MigrationStarter hands a note already claimed with token to a background migrator and returns its run id.
type MigrationStarter func(cb Callbacks, path, token string) (int, error)

// AutoMigrate wraps only the main agent's vault_read, so a migrator's own reads start nothing.
func AutoMigrate(read Handler, start MigrationStarter) Handler {
	if read == nil || start == nil || records.Path() == "" {
		return read
	}
	return func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
		res := read(ctx, call, cb)
		var in struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
		}
		if json.Unmarshal(call.Input, &in) != nil || in.Path == "" || in.Offset > 1 ||
			res.OfToolResult == nil || res.OfToolResult.IsError.Or(false) {
			return res
		}
		path, ok := ledgerPath(in.Path)
		if !ok {
			return res
		}
		store, err := openRecordStore()
		if err != nil {
			return res
		}
		status, rows, err := store.NoteRecords(path)
		if err != nil {
			return res
		}
		var note string
		switch status {
		case "done":
			note = recordsNote(rows)
		case "in_progress":
			note = "[Records store: a migrator is moving this note into records now; do not migrate it yourself.]"
		case "pending":
			if run, err := claimAndStart(store, start, cb, path); err == nil {
				note = fmt.Sprintf("[Records store: this note was not migrated yet, so run %d is migrating it in the background. Nobody needs to wait for it.]", run)
			}
		}
		if note != "" {
			res.OfToolResult.Content = append(res.OfToolResult.Content,
				anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: "\n\n" + note}})
		}
		return res
	}
}

func claimAndStart(store *records.Store, start MigrationStarter, cb Callbacks, path string) (int, error) {
	c, err := store.Claim(path, "main pid "+strconv.Itoa(os.Getpid())+" (auto-migrate)")
	if err != nil {
		return 0, err
	}
	run, err := start(cb, c.Path, c.Token)
	if err != nil {
		_ = store.Finish(c.Path, c.Token, "pending", nil, "")
		return 0, err
	}
	return run, nil
}

// ledgerPath turns a vault_read path into the vault-relative form the ledger keys on.
func ledgerPath(p string) (string, bool) {
	root := vaultRoot()
	if root == "" {
		return "", false
	}
	full, err := vault.Resolve(root, p)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func recordsNote(rows []records.Row) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Records store: this note was migrated. Its records, current versions, read in full with vault_record_read op get:]")
	for _, r := range rows {
		fmt.Fprintf(&b, "\n%s [%s about %s", r.ID, r.Kind, r.Subject)
		if r.Status != "active" {
			b.WriteString(", " + r.Status)
		}
		fmt.Fprintf(&b, "] %s", r.Title)
	}
	return b.String()
}
