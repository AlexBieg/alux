-- A record may replace several; each replaced record has exactly one successor.
CREATE TABLE supersessions (
  predecessor TEXT PRIMARY KEY REFERENCES records(id),
  successor   TEXT NOT NULL REFERENCES records(id),
  tx_id       TEXT NOT NULL REFERENCES txns(id),
  CHECK (predecessor != successor)
) STRICT;

CREATE INDEX supersessions_successor ON supersessions(successor);

INSERT INTO supersessions (predecessor, successor, tx_id)
SELECT supersedes, id, tx_id FROM records WHERE supersedes IS NOT NULL;

CREATE TRIGGER supersessions_no_change BEFORE UPDATE ON supersessions
BEGIN SELECT RAISE(ABORT, 'supersessions are immutable'); END;
CREATE TRIGGER supersessions_no_delete BEFORE DELETE ON supersessions
BEGIN SELECT RAISE(ABORT, 'supersessions are immutable'); END;
CREATE TRIGGER supersessions_guard BEFORE INSERT ON supersessions
BEGIN
  SELECT RAISE(ABORT, 'a record may only supersede one about the same subject')
  WHERE (SELECT h.head FROM records r JOIN entity_head h ON h.id = r.subject_id WHERE r.id = NEW.predecessor)
    IS NOT (SELECT h.head FROM records r JOIN entity_head h ON h.id = r.subject_id WHERE r.id = NEW.successor);
  SELECT RAISE(ABORT, 'a record may only supersede one of the same kind: to answer a question, link it with role answers and mark the question done')
  WHERE (SELECT kind FROM records WHERE id = NEW.predecessor) IS NOT (SELECT kind FROM records WHERE id = NEW.successor);
  SELECT RAISE(ABORT, 'a retracted record cannot be superseded')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.predecessor AND event = 'retracted');
END;

-- records.supersedes still names the first predecessor, so writes from an alux built before this step are mirrored here.
CREATE TRIGGER records_mirror_supersedes AFTER INSERT ON records
WHEN NEW.supersedes IS NOT NULL
BEGIN INSERT INTO supersessions (predecessor, successor, tx_id) VALUES (NEW.supersedes, NEW.id, NEW.tx_id); END;

-- A slot is filing, not content, so a state written without one may be given one once.
CREATE TABLE record_slots (
  record_id TEXT PRIMARY KEY REFERENCES records(id),
  slot      TEXT NOT NULL CHECK (length(trim(slot)) > 0),
  tx_id     TEXT NOT NULL REFERENCES txns(id)
) STRICT;

CREATE TRIGGER record_slots_no_change BEFORE UPDATE ON record_slots
BEGIN SELECT RAISE(ABORT, 'a slot is set once: supersede the record to change it'); END;
CREATE TRIGGER record_slots_no_delete BEFORE DELETE ON record_slots
BEGIN SELECT RAISE(ABORT, 'a slot is set once: supersede the record to change it'); END;
CREATE TRIGGER record_slots_guard BEFORE INSERT ON record_slots
BEGIN
  SELECT RAISE(ABORT, 'only a state can fill a slot')
  WHERE (SELECT kind FROM records WHERE id = NEW.record_id) IS NOT 'state';
  SELECT RAISE(ABORT, 'the record already names its slot in attrs')
  WHERE (SELECT json_extract(attrs, '$.slot') FROM records WHERE id = NEW.record_id) IS NOT NULL;
END;
CREATE TRIGGER record_slots_search AFTER INSERT ON record_slots
BEGIN
  UPDATE records_search SET attrs = trim(coalesce(attrs, '') || ' ' || NEW.slot)
  WHERE rowid = (SELECT rowid FROM records WHERE id = NEW.record_id);
END;

CREATE VIEW record_slot AS
SELECT r.id AS record_id, coalesce(json_extract(r.attrs, '$.slot'), s.slot) AS slot
FROM records r LEFT JOIN record_slots s ON s.record_id = r.id;

-- A replaced record still held until its successor's valid_from; an undated successor replaces it on every date.
DROP VIEW record_status;
CREATE VIEW record_status AS
SELECT r.rowid AS rid, r.*,
  CASE
    WHEN EXISTS (SELECT 1 FROM record_events e WHERE e.record_id = r.id AND e.event = 'retracted') THEN 'retracted'
    WHEN EXISTS (SELECT 1 FROM supersessions s JOIN records n ON n.id = s.successor WHERE s.predecessor = r.id
      AND (n.valid_from IS NULL OR substr(n.valid_from, 1, 10) <= date('now'))) THEN 'superseded'
    WHEN (SELECT e.event FROM record_events e WHERE e.record_id = r.id ORDER BY e.id DESC LIMIT 1) = 'done' THEN 'done'
    WHEN r.valid_until IS NOT NULL AND substr(r.valid_until, 1, 10) < date('now') THEN 'expired'
    WHEN r.valid_from IS NOT NULL AND substr(r.valid_from, 1, 10) > date('now') THEN 'not_yet'
    ELSE 'active'
  END AS status
FROM records r;

DROP TRIGGER events_guard;
CREATE TRIGGER events_guard BEFORE INSERT ON record_events
BEGIN
  SELECT RAISE(ABORT, 'retraction is final')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.record_id AND event = 'retracted');
  SELECT RAISE(ABORT, 'only todos and questions can be done or reopened')
  WHERE NEW.event IN ('done', 'reopened')
    AND (SELECT kind FROM records WHERE id = NEW.record_id) NOT IN ('todo', 'question');
  SELECT RAISE(ABORT, 'a superseded record cannot change status')
  WHERE EXISTS (SELECT 1 FROM supersessions WHERE predecessor = NEW.record_id);
END;
