-- A dropped record was true when written but does not belong in the store; like a retracted one it leaves every read but history, and the drop is final.
DROP VIEW todo_board;
DROP VIEW current_records;
DROP VIEW record_status;
DROP TRIGGER records_supersede_guard;
DROP TRIGGER supersessions_guard;

CREATE TEMP TABLE record_events_copy AS SELECT id, record_id, event, reason, tx_id FROM record_events;
DROP TABLE record_events;
CREATE TABLE record_events (
  id        INTEGER PRIMARY KEY,
  record_id TEXT NOT NULL REFERENCES records(id),
  event     TEXT NOT NULL CHECK (event IN ('done', 'reopened', 'retracted', 'dropped')),
  reason    TEXT NOT NULL CHECK (length(reason) > 0),
  tx_id     TEXT NOT NULL REFERENCES txns(id)
) STRICT;
INSERT INTO record_events (id, record_id, event, reason, tx_id) SELECT id, record_id, event, reason, tx_id FROM record_events_copy;
DROP TABLE record_events_copy;

CREATE TRIGGER events_no_change BEFORE UPDATE ON record_events
BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON record_events
BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;

CREATE TRIGGER events_guard BEFORE INSERT ON record_events
BEGIN
  SELECT RAISE(ABORT, 'retraction is final')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.record_id AND event = 'retracted');
  SELECT RAISE(ABORT, 'a drop is final')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.record_id AND event = 'dropped');
  SELECT RAISE(ABORT, 'only todos and questions can be done or reopened')
  WHERE NEW.event IN ('done', 'reopened')
    AND (SELECT kind FROM records WHERE id = NEW.record_id) NOT IN ('todo', 'question');
  SELECT RAISE(ABORT, 'a superseded record cannot change status')
  WHERE EXISTS (SELECT 1 FROM supersessions WHERE predecessor = NEW.record_id);
END;

CREATE TRIGGER records_supersede_guard BEFORE INSERT ON records
WHEN NEW.supersedes IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'a record may only supersede one about the same subject')
  WHERE (SELECT h.head FROM records r JOIN entity_head h ON h.id = r.subject_id WHERE r.id = NEW.supersedes)
    IS NOT (SELECT head FROM entity_head WHERE id = NEW.subject_id);
  SELECT RAISE(ABORT, 'a record may only supersede one of the same kind: to answer a question, link it with role answers and mark the question done')
  WHERE (SELECT kind FROM records WHERE id = NEW.supersedes) IS NOT NEW.kind;
  SELECT RAISE(ABORT, 'a retracted record cannot be superseded')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.supersedes AND event = 'retracted');
  SELECT RAISE(ABORT, 'a dropped record cannot be superseded')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.supersedes AND event = 'dropped');
END;

CREATE TRIGGER supersessions_guard BEFORE INSERT ON supersessions
BEGIN
  SELECT RAISE(ABORT, 'a record may only supersede one about the same subject')
  WHERE (SELECT h.head FROM records r JOIN entity_head h ON h.id = r.subject_id WHERE r.id = NEW.predecessor)
    IS NOT (SELECT h.head FROM records r JOIN entity_head h ON h.id = r.subject_id WHERE r.id = NEW.successor);
  SELECT RAISE(ABORT, 'a record may only supersede one of the same kind: to answer a question, link it with role answers and mark the question done')
  WHERE (SELECT kind FROM records WHERE id = NEW.predecessor) IS NOT (SELECT kind FROM records WHERE id = NEW.successor);
  SELECT RAISE(ABORT, 'a retracted record cannot be superseded')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.predecessor AND event = 'retracted');
  SELECT RAISE(ABORT, 'a dropped record cannot be superseded')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.predecessor AND event = 'dropped');
END;

CREATE VIEW record_status AS
SELECT r.rowid AS rid, r.*,
  CASE
    WHEN EXISTS (SELECT 1 FROM record_events e WHERE e.record_id = r.id AND e.event = 'retracted') THEN 'retracted'
    WHEN EXISTS (SELECT 1 FROM record_events e WHERE e.record_id = r.id AND e.event = 'dropped') THEN 'dropped'
    WHEN EXISTS (WITH RECURSIVE ahead(id) AS (
        SELECT successor FROM supersessions WHERE predecessor = r.id
        UNION SELECT s.successor FROM supersessions s JOIN ahead a ON s.predecessor = a.id)
      SELECT 1 FROM ahead a JOIN records n ON n.id = a.id
      WHERE n.valid_from IS NULL OR substr(n.valid_from, 1, 10) <= date('now')) THEN 'superseded'
    WHEN (SELECT e.event FROM record_events e WHERE e.record_id = r.id ORDER BY e.id DESC LIMIT 1) = 'done' THEN 'done'
    WHEN r.valid_until IS NOT NULL AND substr(r.valid_until, 1, 10) < date('now') THEN 'expired'
    WHEN r.valid_from IS NOT NULL AND substr(r.valid_from, 1, 10) > date('now') THEN 'not_yet'
    ELSE 'active'
  END AS status
FROM records r;

CREATE VIEW current_records AS SELECT * FROM record_status WHERE status = 'active';

CREATE VIEW todo_board AS
SELECT CASE WHEN json_extract(attrs, '$.due') IS NOT NULL THEN 'scheduled'
            WHEN json_extract(attrs, '$.gate') IS NOT NULL THEN 'gated'
            ELSE 'unscheduled' END AS lane,
       count(*) AS n
FROM current_records WHERE kind = 'todo' GROUP BY lane;
