-- A replaced record still holds until some later version in its chain takes effect; an undated version takes effect on every date.
DROP VIEW record_status;
CREATE VIEW record_status AS
SELECT r.rowid AS rid, r.*,
  CASE
    WHEN EXISTS (SELECT 1 FROM record_events e WHERE e.record_id = r.id AND e.event = 'retracted') THEN 'retracted'
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
