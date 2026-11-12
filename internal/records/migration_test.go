package records

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpeningAVersionOneStoreUpgradesItAndKeepsItsRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema + `; PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEntity(EntityInput{Type: "system", Name: "Harness", Sensitivity: "normal"}, agent); err != nil {
		t.Fatal(err)
	}
	res, err := s.Add(RecordInput{Kind: "claim", Subject: "Harness", Title: "Survives the upgrade", Sensitivity: "normal",
		Source: Source{Type: "conversation", Ref: "test"}}, agent)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := count(t, s, `PRAGMA user_version`); v != schemaVersion {
		t.Fatalf("user_version %d, want %d", v, schemaVersion)
	}
	if n := count(t, s, `SELECT count(*) FROM records WHERE id = ?`, res.ID); n != 1 {
		t.Fatal("record lost across the upgrade")
	}
	if _, _, err := s.SeedMigration([]string{"HOME.md"}); err != nil {
		t.Fatalf("ledger missing after upgrade: %v", err)
	}
}

func TestAStoreFromANewerBuildIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "this build knows") {
		t.Fatalf("want a newer-schema refusal, got %v", err)
	}
}

func seeded(t *testing.T, paths ...string) *Store {
	t.Helper()
	s := open(t)
	if _, _, err := s.SeedMigration(paths); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSeedingIsIdempotentAndReportsVanishedNotes(t *testing.T) {
	s := seeded(t, "a.md", "b.md")
	added, gone, err := s.SeedMigration([]string{"b.md", "c.md"})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || !slices.Equal(gone, []string{"a.md"}) {
		t.Fatalf("added %d gone %v", added, gone)
	}
	st, err := s.MigrationStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 3 || st.Counts["pending"] != 3 {
		t.Fatalf("status %+v", st)
	}
}

func TestConcurrentClaimersNeverGetTheSameNote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	notes := make([]string, 40)
	for i := range notes {
		notes[i] = fmt.Sprintf("Systems/n%02d.md", i)
	}
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, _, err := first.SeedMigration(notes); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := range 4 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c, err := s.Claim("", fmt.Sprintf("worker %d", w))
				if errors.Is(err, ErrNothingToClaim) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				seen[c.Path]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != len(notes) {
		t.Fatalf("claimed %d distinct notes, want %d", len(seen), len(notes))
	}
	for p, n := range seen {
		if n != 1 {
			t.Fatalf("%s claimed %d times", p, n)
		}
	}
}

func TestAnExpiredClaimIsRetakenAndItsHolderCannotFinish(t *testing.T) {
	s := seeded(t, "a.md")
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return t0 }
	t.Cleanup(func() { now = time.Now })

	stale, err := s.Claim("", "slow agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("", "eager agent"); !errors.Is(err, ErrNothingToClaim) {
		t.Fatalf("a live claim was handed out twice: %v", err)
	}

	now = func() time.Time { return t0.Add(ClaimTTL + time.Minute) }
	fresh, err := s.Claim("", "second agent")
	if err != nil || fresh.Path != "a.md" || fresh.Token == stale.Token {
		t.Fatalf("expired claim not retaken: %+v %v", fresh, err)
	}
	if err := s.Finish("a.md", stale.Token, "failed", nil, "gave up"); err == nil || !strings.Contains(err.Error(), "second agent") {
		t.Fatalf("stale holder's finish not refused: %v", err)
	}
	if err := s.Finish("a.md", fresh.Token, "skipped", nil, "archived"); err != nil {
		t.Fatal(err)
	}
}

func TestFinishRules(t *testing.T) {
	s := seeded(t, "a.md", "b.md")
	res, err := s.Add(rec("claim", "Migrated fact", nil), agent)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Claim("a.md", "agent")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Finish("a.md", "", "done", []string{res.ID}, ""); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("finished someone's live claim without its token: %v", err)
	}
	if err := s.Finish("a.md", c.Token, "done", []string{"rec_missing"}, ""); err == nil || !strings.Contains(err.Error(), "rec_missing") {
		t.Fatalf("done accepted an unknown record: %v", err)
	}
	if err := s.Finish("a.md", c.Token, "done", []string{res.ID}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish("a.md", "", "skipped", nil, "changed my mind"); err == nil || !strings.Contains(err.Error(), "already done") {
		t.Fatalf("a done note was overwritten: %v", err)
	}
	if err := s.Finish("a.md", "", "pending", nil, "redo with the newer note"); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	if err := s.Finish("b.md", "", "skipped", nil, "superseded by a.md"); err != nil {
		t.Fatalf("an unclaimed note found while following a chain could not be skipped: %v", err)
	}
	if _, err := s.Claim("b.md", "agent"); err == nil || !strings.Contains(err.Error(), "skipped") {
		t.Fatalf("claimed a skipped note: %v", err)
	}
}
