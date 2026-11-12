-- One row per candidate a writer judged before a staged write went through; the labels a score floor is fitted from.
CREATE TABLE candidate_verdicts (
  id           INTEGER PRIMARY KEY,
  tx_id        TEXT NOT NULL REFERENCES txns(id),
  target       TEXT NOT NULL CHECK (target IN ('record', 'entity')),
  draft        TEXT NOT NULL,
  written_id   TEXT,
  candidate_id TEXT NOT NULL,
  verdict      TEXT NOT NULL CHECK (verdict IN ('same', 'related', 'distinct')),
  rank         INTEGER NOT NULL,
  score        REAL NOT NULL,
  signals      TEXT NOT NULL,
  sensitivity  TEXT NOT NULL CHECK (sensitivity IN ('normal', 'phi'))
) STRICT;
CREATE INDEX candidate_verdicts_written ON candidate_verdicts(written_id);
CREATE TRIGGER candidate_verdicts_no_change BEFORE UPDATE ON candidate_verdicts
BEGIN SELECT RAISE(ABORT, 'verdicts are immutable'); END;
CREATE TRIGGER candidate_verdicts_no_delete BEFORE DELETE ON candidate_verdicts
BEGIN SELECT RAISE(ABORT, 'verdicts are immutable'); END;
