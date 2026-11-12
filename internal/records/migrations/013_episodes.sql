-- One gist per session, written once: records point at it through txns.session, so it outlives the pruned transcript.
CREATE TABLE episodes (
  session     TEXT PRIMARY KEY CHECK (length(trim(session)) > 0),
  title       TEXT NOT NULL CHECK (length(title) BETWEEN 3 AND 200),
  gist        TEXT NOT NULL CHECK (json_valid(gist) AND json_type(gist) = 'object'
                AND json_extract(gist, '$.goal') IS NOT NULL AND json_extract(gist, '$.outcome') IS NOT NULL),
  sensitivity TEXT NOT NULL CHECK (sensitivity IN ('normal', 'phi')),
  tx_id       TEXT NOT NULL REFERENCES txns(id)
) STRICT;

CREATE TRIGGER episodes_no_change BEFORE UPDATE ON episodes
BEGIN SELECT RAISE(ABORT, 'a gist is written once'); END;
CREATE TRIGGER episodes_no_delete BEFORE DELETE ON episodes
BEGIN SELECT RAISE(ABORT, 'a gist is written once'); END;
