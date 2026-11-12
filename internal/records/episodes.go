package records

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/alexbieg/alux/internal/taint"
)

type Gist struct {
	Goal            string   `json:"goal"`
	Outcome         string   `json:"outcome"`
	TurnPoints      []string `json:"turn_points,omitempty"`
	Surprises       []string `json:"surprises,omitempty"`
	TriedAndDropped []string `json:"tried_and_dropped,omitempty"`
	Artifacts       []string `json:"artifacts,omitempty"`
}

type EpisodeInput struct {
	Session     string `json:"session"`
	Title       string `json:"title"`
	Gist        Gist   `json:"gist"`
	Sensitivity string `json:"sensitivity"`
}

type Episode struct {
	EpisodeInput
	RecordedAt string
	Records    []Row
}

// WriteEpisode refuses a session that wrote no record, so a gist always has records to hang from.
func (s *Store) WriteEpisode(in EpisodeInput, a Actor) error {
	in.Session, in.Title = strings.TrimSpace(in.Session), strings.TrimSpace(in.Title)
	switch {
	case in.Session == "":
		return errors.New("episode needs the session it gists")
	case strings.TrimSpace(in.Gist.Goal) == "" || strings.TrimSpace(in.Gist.Outcome) == "":
		return errors.New("a gist needs goal and outcome")
	case in.Sensitivity != "normal" && in.Sensitivity != "phi":
		return errors.New("episode needs sensitivity normal or phi")
	}
	gist, err := json.Marshal(in.Gist)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var wrote, gisted bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM records r JOIN txns t ON t.id = r.tx_id WHERE t.session = ?1),
		EXISTS (SELECT 1 FROM episodes WHERE session = ?1)`, in.Session).Scan(&wrote, &gisted); err != nil {
		return err
	}
	switch {
	case gisted:
		return fmt.Errorf("session %s already has a gist", in.Session)
	case !wrote:
		return fmt.Errorf("session %s wrote no records", in.Session)
	}
	txID, err := insertTxn(tx, a)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO episodes (session, title, gist, sensitivity, tx_id) VALUES (?, ?, ?, ?, ?)`,
		in.Session, in.Title, string(gist), in.Sensitivity, txID); err != nil {
		return cleanErr(err)
	}
	return tx.Commit()
}

// GetEpisode returns a session's gist with the current records it wrote.
func (s *Store) GetEpisode(session string) (Episode, error) {
	var e Episode
	var gist string
	err := s.db.QueryRow(`SELECT ep.session, ep.title, ep.gist, ep.sensitivity, t.at FROM episodes ep JOIN txns t ON t.id = ep.tx_id
		WHERE ep.session = ?`, session).Scan(&e.Session, &e.Title, &gist, &e.Sensitivity, &e.RecordedAt)
	if err != nil {
		return Episode{}, fmt.Errorf("no gist for session %q", session)
	}
	if e.Sensitivity == "phi" {
		taint.Set()
	}
	if err := json.Unmarshal([]byte(gist), &e.Gist); err != nil {
		return Episode{}, err
	}
	e.Records, err = s.rows(fmt.Sprintf(`SELECT %s FROM current_records r %s JOIN txns t ON t.id = r.tx_id
		WHERE t.session = ? ORDER BY r.rid`, rowColumns, subjectJoin), session)
	return e, err
}

// UngistedSessions lists sessions that wrote records but have no gist, newest first.
func (s *Store) UngistedSessions() ([]string, error) {
	return s.strings(`SELECT t.session FROM records r JOIN txns t ON t.id = r.tx_id
		WHERE t.session IS NOT NULL AND NOT EXISTS (SELECT 1 FROM episodes ep WHERE ep.session = t.session)
		GROUP BY t.session ORDER BY max(t.at) DESC`)
}
