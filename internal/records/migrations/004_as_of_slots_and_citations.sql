-- A state may name the slot it fills; the write path keeps one current record per subject, kind and slot.
INSERT INTO kind_fields (kind, field, type, required, enum_values) VALUES ('state', 'slot', 'text', 0, NULL);

-- Further sources for a record's content; one row per source, so citing the same source twice counts once.
CREATE TABLE record_sources (
  record_id TEXT NOT NULL REFERENCES records(id),
  source_id TEXT NOT NULL REFERENCES sources(id),
  tx_id     TEXT NOT NULL REFERENCES txns(id),
  PRIMARY KEY (record_id, source_id)
) STRICT;

CREATE TRIGGER record_sources_no_change BEFORE UPDATE ON record_sources
BEGIN SELECT RAISE(ABORT, 'citations are immutable'); END;
CREATE TRIGGER record_sources_no_delete BEFORE DELETE ON record_sources
BEGIN SELECT RAISE(ABORT, 'citations are immutable'); END;
CREATE TRIGGER record_sources_guard BEFORE INSERT ON record_sources
BEGIN
  SELECT RAISE(ABORT, 'that source is already the record''s own source')
  WHERE (SELECT derived_from FROM records WHERE id = NEW.record_id) = NEW.source_id;
END;

-- A record whose valid_from is still ahead is not yet true.
DROP VIEW record_status;
CREATE VIEW record_status AS
SELECT r.rowid AS rid, r.*,
  CASE
    WHEN EXISTS (SELECT 1 FROM record_events e WHERE e.record_id = r.id AND e.event = 'retracted') THEN 'retracted'
    WHEN EXISTS (SELECT 1 FROM records s WHERE s.supersedes = r.id) THEN 'superseded'
    WHEN (SELECT e.event FROM record_events e WHERE e.record_id = r.id ORDER BY e.id DESC LIMIT 1) = 'done' THEN 'done'
    WHEN r.valid_until IS NOT NULL AND substr(r.valid_until, 1, 10) < date('now') THEN 'expired'
    WHEN r.valid_from IS NOT NULL AND substr(r.valid_from, 1, 10) > date('now') THEN 'not_yet'
    ELSE 'active'
  END AS status
FROM records r;
