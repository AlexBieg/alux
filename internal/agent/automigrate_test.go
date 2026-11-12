package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/records"
	"github.com/anthropics/anthropic-sdk-go"
)

func autoMigrateFixture(t *testing.T, notes ...string) *records.Store {
	t.Helper()
	t.Setenv("ALUX_RECORDS", filepath.Join(t.TempDir(), "records.db"))
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orig := vaultRoot
	vaultRoot = func() string { return root }
	t.Cleanup(func() { vaultRoot = orig })
	store, err := openRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SeedMigration(notes); err != nil {
		t.Fatal(err)
	}
	return store
}

func fakeRead(_ context.Context, call Call, _ Callbacks) anthropic.ContentBlockParamUnion {
	return ok(call.ID, "note text")
}

func readThrough(t *testing.T, h Handler, input map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(input)
	res := h(context.Background(), Call{ID: "c", Name: "vault_read", Input: raw}, Callbacks{})
	var text string
	for _, c := range res.OfToolResult.Content {
		if c.OfText != nil {
			text += c.OfText.Text
		}
	}
	return text
}

type starts struct{ paths, tokens []string }

func (s *starts) start(_ Callbacks, path, token string) (int, error) {
	s.paths = append(s.paths, path)
	s.tokens = append(s.tokens, token)
	return len(s.paths), nil
}

func TestReadingAPendingNoteHandsItToOneMigratorWithItsClaim(t *testing.T) {
	store := autoMigrateFixture(t, "Systems/A.md", "Systems/B.md")
	var got starts
	h := AutoMigrate(fakeRead, got.start)

	out := readThrough(t, h, map[string]any{"path": "Systems/A.md"})
	if !strings.HasPrefix(out, "note text") || !strings.Contains(out, "run 1 is migrating it") {
		t.Fatalf("read should return the note and say a migration started: %q", out)
	}
	if out := readThrough(t, h, map[string]any{"path": "./Systems/A.md", "section": "Why"}); !strings.Contains(out, "do not migrate it yourself") {
		t.Fatalf("a second read of a claimed note should say it is in progress: %q", out)
	}
	readThrough(t, h, map[string]any{"query": "anything"})
	readThrough(t, h, map[string]any{"path": "Systems/B.md", "offset": 40})
	if len(got.paths) != 1 || got.paths[0] != "Systems/A.md" {
		t.Fatalf("want exactly one start, for Systems/A.md; got %v", got.paths)
	}
	if err := store.Finish("Systems/A.md", got.tokens[0], "skipped", nil, "test"); err != nil {
		t.Fatalf("the migrator should hold the live claim: %v", err)
	}
}

func TestAFailedStartHandsTheClaimBack(t *testing.T) {
	store := autoMigrateFixture(t, "A.md")
	h := AutoMigrate(fakeRead, func(Callbacks, string, string) (int, error) { return 0, errors.New("no slot") })
	if out := readThrough(t, h, map[string]any{"path": "A.md"}); out != "note text" {
		t.Fatalf("a failed start should leave the read untouched: %q", out)
	}
	if st, _, _ := store.NoteRecords("A.md"); st != "pending" {
		t.Fatalf("note should be pending again, is %q", st)
	}
}

func TestAMigratedNoteIsReadBesideTheCurrentVersionOfItsRecords(t *testing.T) {
	store := autoMigrateFixture(t, "A.md")
	actor := records.Actor{Kind: "agent", Agent: "test"}
	if _, err := store.AddEntity(records.EntityInput{Type: "system", Name: "alux", Sensitivity: "normal"}, actor); err != nil {
		t.Fatal(err)
	}
	add := func(title, supersedes string) string {
		res, err := store.Add(records.RecordInput{Kind: "claim", Subject: "alux", Title: title, Sensitivity: "normal",
			Supersedes: records.IDs{supersedes}, Source: records.Source{Type: "file", Ref: "A.md"}}, actor)
		if err != nil {
			t.Fatal(err)
		}
		return res.ID
	}
	old := add("First wording", "")
	c, _ := store.Claim("A.md", "test")
	if err := store.Finish("A.md", c.Token, "done", []string{old}, ""); err != nil {
		t.Fatal(err)
	}
	current := add("Corrected wording", old)

	var got starts
	out := readThrough(t, AutoMigrate(fakeRead, got.start), map[string]any{"path": "A.md"})
	if !strings.HasPrefix(out, "note text") || !strings.Contains(out, current+" [claim about alux] Corrected wording") || strings.Contains(out, "First wording") {
		t.Fatalf("read should show the note then the record's current version: %q", out)
	}
	if len(got.paths) != 0 {
		t.Fatalf("a migrated note started a migration: %v", got.paths)
	}
}

func TestAutoMigrateIsOnOnlyWhenSettingsSayTrue(t *testing.T) {
	for settings, want := range map[string]bool{
		"":                        false,
		`{}`:                      false,
		`{"auto_migrate": false}`: false,
		`{"auto_migrate": true}`:  true,
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if settings != "" {
			dir := filepath.Join(home, paths.Config)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := AutoMigrateOn(); got != want {
			t.Errorf("settings %q: got %v, want %v", settings, got, want)
		}
	}
}
