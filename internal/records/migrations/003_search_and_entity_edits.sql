-- A merged entity's name, aliases, records and links all answer to the entity it was merged into.
CREATE TABLE entity_merges (
  from_entity TEXT PRIMARY KEY REFERENCES entities(id),
  into_entity TEXT NOT NULL REFERENCES entities(id),
  reason      TEXT NOT NULL CHECK (length(trim(reason)) > 0),
  tx_id       TEXT NOT NULL REFERENCES txns(id),
  CHECK (from_entity != into_entity)
) STRICT;

CREATE TRIGGER entity_merges_no_change BEFORE UPDATE ON entity_merges
BEGIN SELECT RAISE(ABORT, 'merges are final'); END;
CREATE TRIGGER entity_merges_no_delete BEFORE DELETE ON entity_merges
BEGIN SELECT RAISE(ABORT, 'merges are final'); END;

-- Merges stay one hop deep, so the head of any entity is a single lookup.
CREATE TRIGGER entity_merges_guard BEFORE INSERT ON entity_merges
BEGIN
  SELECT RAISE(ABORT, 'cannot merge into an entity that was itself merged: merge into the entity it now answers to')
  WHERE EXISTS (SELECT 1 FROM entity_merges WHERE from_entity = NEW.into_entity);
  SELECT RAISE(ABORT, 'cannot merge an entity others were merged into: merge the other entity into it instead')
  WHERE EXISTS (SELECT 1 FROM entity_merges WHERE into_entity = NEW.from_entity);
  SELECT RAISE(ABORT, 'cannot merge entities of different sensitivity')
  WHERE (SELECT sensitivity FROM entities WHERE id = NEW.from_entity) IS NOT (SELECT sensitivity FROM entities WHERE id = NEW.into_entity);
END;

CREATE VIEW entity_head AS
SELECT e.id, coalesce(m.into_entity, e.id) AS head
FROM entities e LEFT JOIN entity_merges m ON m.from_entity = e.id;

-- Every name a head answers to: its own and its merged entities' names and aliases.
CREATE VIEW entity_names AS
SELECT head AS entity_id, group_concat(name, ' ') AS names FROM (
  SELECT h.head, e.name FROM entity_head h JOIN entities e ON e.id = h.id
  UNION ALL
  SELECT h.head, a.alias FROM entity_head h JOIN entity_aliases a ON a.entity_id = h.id)
GROUP BY head;

DROP TRIGGER records_supersede_guard;
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
END;

-- Search covers attrs and the subject's names; the subject column is refreshed when an alias or merge lands.
DROP TRIGGER records_fts_insert;
DROP TABLE records_fts;
CREATE VIRTUAL TABLE records_search USING fts5(title, text, body, attrs, subject, tokenize = 'porter unicode61');

CREATE TRIGGER records_search_insert AFTER INSERT ON records
BEGIN
  INSERT INTO records_search (rowid, title, text, body, attrs, subject) VALUES (NEW.rowid, NEW.title, NEW.text, NEW.body,
    (SELECT group_concat(atom, ' ') FROM json_tree(NEW.attrs) WHERE type IN ('text', 'integer', 'real') AND atom NOT LIKE 'ent\_%' ESCAPE '\'),
    (SELECT names FROM entity_names WHERE entity_id = (SELECT head FROM entity_head WHERE id = NEW.subject_id)));
END;

CREATE TRIGGER records_search_alias AFTER INSERT ON entity_aliases
BEGIN
  UPDATE records_search SET subject = (SELECT names FROM entity_names WHERE entity_id = (SELECT head FROM entity_head WHERE id = NEW.entity_id))
  WHERE rowid IN (SELECT r.rowid FROM records r JOIN entity_head h ON h.id = r.subject_id
    WHERE h.head = (SELECT head FROM entity_head WHERE id = NEW.entity_id));
END;

CREATE TRIGGER records_search_merge AFTER INSERT ON entity_merges
BEGIN
  UPDATE records_search SET subject = (SELECT names FROM entity_names WHERE entity_id = NEW.into_entity)
  WHERE rowid IN (SELECT r.rowid FROM records r JOIN entity_head h ON h.id = r.subject_id WHERE h.head = NEW.into_entity);
END;

INSERT INTO records_search (rowid, title, text, body, attrs, subject)
SELECT r.rowid, r.title, r.text, r.body,
  (SELECT group_concat(atom, ' ') FROM json_tree(r.attrs) WHERE type IN ('text', 'integer', 'real') AND atom NOT LIKE 'ent\_%' ESCAPE '\'),
  (SELECT names FROM entity_names WHERE entity_id = (SELECT head FROM entity_head WHERE id = r.subject_id))
FROM records r;
