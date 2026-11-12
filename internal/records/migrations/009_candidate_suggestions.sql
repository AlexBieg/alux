-- One row per candidate shown as a hint after a write; whether the writer then linked it is read from links.
CREATE TABLE candidate_suggestions (
  id           INTEGER PRIMARY KEY,
  tx_id        TEXT NOT NULL REFERENCES txns(id),
  target       TEXT NOT NULL CHECK (target IN ('record', 'entity')),
  draft        TEXT NOT NULL,
  written_id   TEXT NOT NULL,
  candidate_id TEXT NOT NULL,
  rank         INTEGER NOT NULL,
  score        REAL NOT NULL,
  signals      TEXT NOT NULL,
  features     TEXT NOT NULL CHECK (json_valid(features)),
  sensitivity  TEXT NOT NULL CHECK (sensitivity IN ('normal', 'phi'))
) STRICT;
CREATE INDEX candidate_suggestions_written ON candidate_suggestions(written_id);
CREATE TRIGGER candidate_suggestions_no_change BEFORE UPDATE ON candidate_suggestions
BEGIN SELECT RAISE(ABORT, 'suggestions are immutable'); END;
CREATE TRIGGER candidate_suggestions_no_delete BEFORE DELETE ON candidate_suggestions
BEGIN SELECT RAISE(ABORT, 'suggestions are immutable'); END;
