package records

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A claim older than this is abandoned and may be taken again.
const ClaimTTL = time.Hour

const stampLayout = "2006-01-02T15:04:05.000Z"

var now = time.Now

var ErrNothingToClaim = errors.New("nothing left to claim: every note is done, skipped, failed or freshly claimed")

func stamp(t time.Time) string { return t.UTC().Format(stampLayout) }

type MigrationClaim struct {
	Path  string
	Token string
}

type ClaimedNote struct {
	Path    string
	By      string
	At      string
	Expired bool
}

type FailedNote struct {
	Path string
	Note string
}

type MigrationStatus struct {
	Counts     map[string]int
	Total      int
	InProgress []ClaimedNote
	Failed     []FailedNote
}

// Seeding is idempotent; gone lists ledger paths no longer present in the vault.
func (s *Store) SeedMigration(paths []string) (added int, gone []string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	present := map[string]bool{}
	for _, p := range paths {
		present[p] = true
		res, err := tx.Exec(`INSERT OR IGNORE INTO vault_migration (path) VALUES (?)`, p)
		if err != nil {
			return 0, nil, err
		}
		n, _ := res.RowsAffected()
		added += int(n)
	}
	rows, err := tx.Query(`SELECT path FROM vault_migration ORDER BY path`)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, nil, err
		}
		if !present[p] {
			gone = append(gone, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return added, gone, tx.Commit()
}

// With path empty, the next pending note in path order is claimed; an expired claim counts as pending.
func (s *Store) Claim(path, by string) (MigrationClaim, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return MigrationClaim{}, err
	}
	defer tx.Rollback()
	t := now()
	cutoff := stamp(t.Add(-ClaimTTL))
	claimable := `(status = 'pending' OR (status = 'in_progress' AND claimed_at < ?))`
	var got string
	if path == "" {
		err = tx.QueryRow(`SELECT path FROM vault_migration WHERE `+claimable+` ORDER BY path LIMIT 1`, cutoff).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			return MigrationClaim{}, ErrNothingToClaim
		}
	} else {
		err = tx.QueryRow(`SELECT path FROM vault_migration WHERE path = ? AND `+claimable, path, cutoff).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			r, ferr := loadLedgerRow(tx, path)
			if ferr != nil {
				return MigrationClaim{}, ferr
			}
			return MigrationClaim{}, fmt.Errorf("%s is %s, not claimable%s", path, r.status, r.claimedSuffix())
		}
	}
	if err != nil {
		return MigrationClaim{}, err
	}
	token := newID("clm_")
	if _, err := tx.Exec(`UPDATE vault_migration SET status = 'in_progress', claim = ?, claimed_by = ?, claimed_at = ? WHERE path = ?`,
		token, null(by), stamp(t), got); err != nil {
		return MigrationClaim{}, err
	}
	return MigrationClaim{Path: got, Token: token}, tx.Commit()
}

// Without the claim's token, only an unclaimed note can be finished, and a finished one only reopened.
func (s *Store) Finish(path, token, status string, recordIDs []string, note string) error {
	if !slices.Contains([]string{"done", "skipped", "failed", "pending"}, status) {
		return fmt.Errorf("status must be done, skipped, failed or pending, not %q", status)
	}
	note = strings.TrimSpace(note)
	if (status == "skipped" || status == "failed") && note == "" {
		return fmt.Errorf("a %s note needs a reason in note", status)
	}
	if status == "done" && len(recordIDs) == 0 {
		return errors.New("a done note needs the ids of the records it became; use skipped if it became none")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := loadLedgerRow(tx, path)
	if err != nil {
		return err
	}
	expired := r.status == "in_progress" && r.claimedAt < stamp(now().Add(-ClaimTTL))
	switch {
	case token != "":
		if r.status != "in_progress" || r.claim != token {
			return fmt.Errorf("your claim on %s is gone: it is now %s%s; nothing was written", path, r.status, r.claimedSuffix())
		}
	case r.status == "pending", expired:
	case r.status == "in_progress":
		return fmt.Errorf("%s is claimed%s: finish it with that claim's token, or wait for the claim to expire", path, r.claimedSuffix())
	default:
		if status != "pending" {
			return fmt.Errorf("%s is already %s: set it back to pending first if it needs redoing", path, r.status)
		}
	}
	for _, id := range recordIDs {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM records WHERE id = ?)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("no record %q: store the records first, then finish the note", id)
		}
	}
	if recordIDs == nil {
		recordIDs = []string{}
	}
	ids, err := json.Marshal(recordIDs)
	if err != nil {
		return err
	}
	var finished any
	if status != "pending" {
		finished = stamp(now())
	}
	_, err = tx.Exec(`UPDATE vault_migration SET status = ?, claim = NULL, claimed_by = NULL, claimed_at = NULL,
		finished_at = ?, records = ?, note = ? WHERE path = ?`, status, finished, string(ids), null(note), path)
	if err != nil {
		return cleanErr(err)
	}
	return tx.Commit()
}

func (s *Store) MigrationStatus() (MigrationStatus, error) {
	st := MigrationStatus{Counts: map[string]int{}}
	rows, err := s.db.Query(`SELECT status, count(*) FROM vault_migration GROUP BY status`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return st, err
		}
		st.Counts[status] = n
		st.Total += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}
	cutoff := stamp(now().Add(-ClaimTTL))
	rows, err = s.db.Query(`SELECT path, coalesce(claimed_by, ''), claimed_at, claimed_at < ? FROM vault_migration
		WHERE status = 'in_progress' ORDER BY claimed_at`, cutoff)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var c ClaimedNote
		if err := rows.Scan(&c.Path, &c.By, &c.At, &c.Expired); err != nil {
			rows.Close()
			return st, err
		}
		st.InProgress = append(st.InProgress, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}
	rows, err = s.db.Query(`SELECT path, coalesce(note, '') FROM vault_migration WHERE status = 'failed' ORDER BY path LIMIT 20`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FailedNote
		if err := rows.Scan(&f.Path, &f.Note); err != nil {
			return st, err
		}
		st.Failed = append(st.Failed, f)
	}
	return st, rows.Err()
}

// NoteRecords is a note's ledger status and the current version of each record it became; status is empty for a note the ledger lacks.
func (s *Store) NoteRecords(path string) (string, []Row, error) {
	var status, raw string
	err := s.db.QueryRow(`SELECT status, records FROM vault_migration WHERE path = ?`, path).Scan(&status, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return "", nil, err
	}
	var out []Row
	seen := map[string]bool{}
	for _, id := range ids {
		head := id
		for {
			var next string
			err := s.db.QueryRow(`SELECT successor FROM supersessions WHERE predecessor = ?`, head).Scan(&next)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return "", nil, err
			}
			head = next
		}
		if seen[head] {
			continue
		}
		seen[head] = true
		rows, err := s.rows(fmt.Sprintf(`SELECT %s FROM record_status r %s WHERE r.id = ?`, rowColumns, subjectJoin), head)
		if err != nil {
			return "", nil, err
		}
		out = append(out, rows...)
	}
	return status, out, nil
}

type ledgerRow struct {
	status, claim, claimedBy, claimedAt string
}

func (r ledgerRow) claimedSuffix() string {
	if r.status != "in_progress" {
		return ""
	}
	return fmt.Sprintf(" by %s at %s", or(r.claimedBy, "an unnamed agent"), r.claimedAt)
}

func loadLedgerRow(q querier, path string) (ledgerRow, error) {
	var r ledgerRow
	err := q.QueryRow(`SELECT status, coalesce(claim, ''), coalesce(claimed_by, ''), coalesce(claimed_at, '')
		FROM vault_migration WHERE path = ?`, path).Scan(&r.status, &r.claim, &r.claimedBy, &r.claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%s is not in the migration ledger: check the path, or seed again", path)
	}
	return r, err
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
