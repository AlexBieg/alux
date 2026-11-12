CREATE TABLE txns (
  id      TEXT PRIMARY KEY CHECK (id LIKE 'tx_%'),
  at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  actor   TEXT NOT NULL CHECK (actor IN ('user', 'agent', 'import')),
  agent   TEXT,
  model   TEXT,
  session TEXT,
  note    TEXT
) STRICT;

-- Where a record's content came from, as distinct from who recorded it.
CREATE TABLE sources (
  id          TEXT PRIMARY KEY CHECK (id LIKE 'src_%'),
  type        TEXT NOT NULL CHECK (type IN ('conversation', 'meeting', 'pr', 'ticket', 'url', 'file', 'observation')),
  ref         TEXT NOT NULL,
  title       TEXT,
  occurred_at TEXT,
  tx_id       TEXT NOT NULL REFERENCES txns(id),
  UNIQUE (type, ref)
) STRICT;

CREATE TABLE entities (
  id          TEXT PRIMARY KEY CHECK (id LIKE 'ent_%'),
  type        TEXT NOT NULL CHECK (type IN ('person', 'system', 'ticket', 'project', 'repo', 'term', 'team', 'org', 'topic')),
  name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
  description TEXT CHECK (length(description) <= 300),
  sensitivity TEXT NOT NULL CHECK (sensitivity IN ('normal', 'phi')),
  tx_id       TEXT NOT NULL REFERENCES txns(id)
) STRICT;

CREATE TABLE entity_aliases (
  alias     TEXT PRIMARY KEY COLLATE NOCASE,
  entity_id TEXT NOT NULL REFERENCES entities(id),
  tx_id     TEXT NOT NULL REFERENCES txns(id)
) STRICT;

CREATE TABLE records (
  id           TEXT PRIMARY KEY CHECK (id LIKE 'rec_%'),
  kind         TEXT NOT NULL CHECK (kind IN ('claim', 'state', 'decision', 'measurement', 'todo', 'question', 'procedure', 'rule', 'argument')),
  subject_id   TEXT NOT NULL REFERENCES entities(id),
  title        TEXT NOT NULL CHECK (length(title) BETWEEN 3 AND 200),
  text         TEXT CHECK (length(text) <= 600),
  body         TEXT CHECK (body IS NULL OR kind IN ('argument', 'procedure')),
  attrs        TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attrs) AND json_type(attrs) = 'object'),
  valid_from   TEXT CHECK (valid_from IS NULL OR valid_from GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'),
  valid_until  TEXT CHECK (valid_until IS NULL OR valid_until GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'),
  supersedes   TEXT REFERENCES records(id),
  confidence   TEXT NOT NULL DEFAULT 'high' CHECK (confidence IN ('high', 'medium', 'low')),
  sensitivity  TEXT NOT NULL CHECK (sensitivity IN ('normal', 'phi')),
  derived_from TEXT NOT NULL REFERENCES sources(id),
  tx_id        TEXT NOT NULL REFERENCES txns(id),
  CHECK (valid_until IS NULL OR valid_from IS NULL OR valid_until >= valid_from),
  CHECK (kind != 'decision' OR json_extract(attrs, '$.chosen') IS NOT NULL),
  CHECK (kind != 'measurement' OR (json_extract(attrs, '$.metric') IS NOT NULL AND json_extract(attrs, '$.value') IS NOT NULL)),
  CHECK (kind != 'argument' OR body IS NOT NULL)
) STRICT;

CREATE UNIQUE INDEX records_one_successor ON records(supersedes) WHERE supersedes IS NOT NULL;
CREATE INDEX records_subject ON records(subject_id, kind);

-- Status changes are appended, never written onto the record.
CREATE TABLE record_events (
  id        INTEGER PRIMARY KEY,
  record_id TEXT NOT NULL REFERENCES records(id),
  event     TEXT NOT NULL CHECK (event IN ('done', 'reopened', 'retracted')),
  reason    TEXT NOT NULL CHECK (length(reason) > 0),
  tx_id     TEXT NOT NULL REFERENCES txns(id)
) STRICT;

CREATE TABLE links (
  id          INTEGER PRIMARY KEY,
  from_record TEXT NOT NULL REFERENCES records(id),
  to_record   TEXT REFERENCES records(id),
  to_entity   TEXT REFERENCES entities(id),
  role        TEXT NOT NULL CHECK (role IN ('about', 'supports', 'contradicts', 'refines', 'depends_on', 'answers', 'see_also')),
  tx_id       TEXT NOT NULL REFERENCES txns(id),
  CHECK ((to_record IS NULL) != (to_entity IS NULL)),
  CHECK (to_record IS NULL OR to_record != from_record)
) STRICT;

CREATE UNIQUE INDEX links_unique_record ON links(from_record, to_record, role) WHERE to_record IS NOT NULL;
CREATE UNIQUE INDEX links_unique_entity ON links(from_record, to_entity, role) WHERE to_entity IS NOT NULL;

-- The write tool validates attrs against this; fields that recur under attrs.extra get promoted here.
CREATE TABLE kind_fields (
  kind        TEXT NOT NULL,
  field       TEXT NOT NULL,
  type        TEXT NOT NULL CHECK (type IN ('text', 'date', 'number', 'entity', 'enum', 'list')),
  required    INTEGER NOT NULL DEFAULT 0 CHECK (required IN (0, 1)),
  enum_values TEXT CHECK (enum_values IS NULL OR json_valid(enum_values)),
  PRIMARY KEY (kind, field)
) STRICT;

INSERT INTO kind_fields (kind, field, type, required, enum_values) VALUES
  ('claim',       'consequence',  'text',   0, NULL),
  ('claim',       'workaround',   'text',   0, NULL),
  ('claim',       'verified_how', 'text',   0, NULL),
  ('state',       'value',        'text',   1, NULL),
  ('state',       'as_of',        'date',   1, NULL),
  ('decision',    'chosen',       'text',   1, NULL),
  ('decision',    'rejected',     'list',   0, NULL),
  ('decision',    'decided_by',   'entity', 0, NULL),
  ('measurement', 'metric',       'text',   1, NULL),
  ('measurement', 'value',        'number', 1, NULL),
  ('measurement', 'unit',         'text',   0, NULL),
  ('measurement', 'n',            'number', 0, NULL),
  ('measurement', 'method',       'text',   0, NULL),
  ('todo',        'gate',         'text',   0, NULL),
  ('todo',        'due',          'date',   0, NULL),
  ('todo',        'owner',        'entity', 0, NULL),
  ('todo',        'priority',     'enum',   0, '["p1","p2","p3"]'),
  ('question',    'options',      'list',   0, NULL),
  ('question',    'owner',        'entity', 0, NULL),
  ('procedure',   'when',         'text',   0, NULL),
  ('procedure',   'steps',        'list',   0, NULL),
  ('rule',        'scope',        'text',   0, NULL),
  ('rule',        'how_to_apply', 'text',   0, NULL),
  ('argument',    'thesis',       'text',   1, NULL);

CREATE TRIGGER records_no_update BEFORE UPDATE ON records
BEGIN SELECT RAISE(ABORT, 'records are immutable: write a record that supersedes it'); END;
CREATE TRIGGER records_no_delete BEFORE DELETE ON records
BEGIN SELECT RAISE(ABORT, 'records are never deleted: retract it'); END;
CREATE TRIGGER events_no_change BEFORE UPDATE ON record_events
BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON record_events
BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;
CREATE TRIGGER links_no_change BEFORE UPDATE ON links
BEGIN SELECT RAISE(ABORT, 'links are immutable'); END;
CREATE TRIGGER links_no_delete BEFORE DELETE ON links
BEGIN SELECT RAISE(ABORT, 'links are immutable'); END;
CREATE TRIGGER txns_no_change BEFORE UPDATE ON txns
BEGIN SELECT RAISE(ABORT, 'txns are immutable'); END;
CREATE TRIGGER txns_no_delete BEFORE DELETE ON txns
BEGIN SELECT RAISE(ABORT, 'txns are immutable'); END;
CREATE TRIGGER entities_no_delete BEFORE DELETE ON entities
BEGIN SELECT RAISE(ABORT, 'entities are never deleted'); END;
CREATE TRIGGER entities_only_description BEFORE UPDATE ON entities
WHEN NEW.id IS NOT OLD.id OR NEW.type IS NOT OLD.type OR NEW.name IS NOT OLD.name
  OR NEW.sensitivity IS NOT OLD.sensitivity
BEGIN SELECT RAISE(ABORT, 'only an entity''s description may change: add an alias to rename'); END;

CREATE TRIGGER records_supersede_guard BEFORE INSERT ON records
WHEN NEW.supersedes IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'a record may only supersede one about the same subject')
  WHERE (SELECT subject_id FROM records WHERE id = NEW.supersedes) IS NOT NEW.subject_id;
  SELECT RAISE(ABORT, 'a record may only supersede one of the same kind: to answer a question, link it with role answers and mark the question done')
  WHERE (SELECT kind FROM records WHERE id = NEW.supersedes) IS NOT NEW.kind;
  SELECT RAISE(ABORT, 'a retracted record cannot be superseded')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.supersedes AND event = 'retracted');
END;

CREATE TRIGGER records_inherit_phi BEFORE INSERT ON records
WHEN NEW.sensitivity = 'normal'
  AND (SELECT sensitivity FROM entities WHERE id = NEW.subject_id) = 'phi'
BEGIN SELECT RAISE(ABORT, 'a record about a phi entity must be marked phi'); END;

CREATE TRIGGER events_guard BEFORE INSERT ON record_events
BEGIN
  SELECT RAISE(ABORT, 'retraction is final')
  WHERE EXISTS (SELECT 1 FROM record_events WHERE record_id = NEW.record_id AND event = 'retracted');
  SELECT RAISE(ABORT, 'only todos and questions can be done or reopened')
  WHERE NEW.event IN ('done', 'reopened')
    AND (SELECT kind FROM records WHERE id = NEW.record_id) NOT IN ('todo', 'question');
  SELECT RAISE(ABORT, 'a superseded record cannot change status')
  WHERE EXISTS (SELECT 1 FROM records WHERE supersedes = NEW.record_id);
END;

-- Things an agent wanted to store and could not. Never read as knowledge; reviewed later.
CREATE TABLE unfiled (
  id          INTEGER PRIMARY KEY,
  reason      TEXT NOT NULL CHECK (reason IN ('rejected', 'no_kind_fits', 'no_subject', 'unsure', 'other')),
  error       TEXT,
  payload     TEXT NOT NULL CHECK (json_valid(payload)),
  summary     TEXT NOT NULL CHECK (length(summary) BETWEEN 3 AND 300),
  sensitivity TEXT NOT NULL CHECK (sensitivity IN ('normal', 'phi')),
  tx_id       TEXT NOT NULL REFERENCES txns(id),
  CHECK (reason != 'rejected' OR error IS NOT NULL)
) STRICT;

CREATE TABLE unfiled_resolutions (
  unfiled_id INTEGER PRIMARY KEY REFERENCES unfiled(id),
  outcome    TEXT NOT NULL CHECK (outcome IN ('filed', 'discarded', 'schema_changed')),
  record_id  TEXT REFERENCES records(id),
  note       TEXT,
  tx_id      TEXT NOT NULL REFERENCES txns(id),
  CHECK (outcome != 'filed' OR record_id IS NOT NULL)
) STRICT;

CREATE TRIGGER unfiled_no_change BEFORE UPDATE ON unfiled
BEGIN SELECT RAISE(ABORT, 'unfiled entries are immutable: resolve them'); END;
CREATE TRIGGER unfiled_no_delete BEFORE DELETE ON unfiled
BEGIN SELECT RAISE(ABORT, 'unfiled entries are immutable: resolve them'); END;
CREATE TRIGGER unfiled_res_no_change BEFORE UPDATE ON unfiled_resolutions
BEGIN SELECT RAISE(ABORT, 'resolutions are immutable'); END;
CREATE TRIGGER unfiled_res_no_delete BEFORE DELETE ON unfiled_resolutions
BEGIN SELECT RAISE(ABORT, 'resolutions are immutable'); END;

CREATE VIEW unfiled_open AS
SELECT u.* FROM unfiled u
WHERE NOT EXISTS (SELECT 1 FROM unfiled_resolutions r WHERE r.unfiled_id = u.id);

CREATE VIEW unfiled_by_reason AS
SELECT reason, count(*) AS n FROM unfiled_open GROUP BY reason;

-- Status is derived, never stored.
CREATE VIEW record_status AS
SELECT r.rowid AS rid, r.*,
  CASE
    WHEN EXISTS (SELECT 1 FROM record_events e WHERE e.record_id = r.id AND e.event = 'retracted') THEN 'retracted'
    WHEN EXISTS (SELECT 1 FROM records s WHERE s.supersedes = r.id) THEN 'superseded'
    WHEN (SELECT e.event FROM record_events e WHERE e.record_id = r.id ORDER BY e.id DESC LIMIT 1) = 'done' THEN 'done'
    WHEN r.valid_until IS NOT NULL AND r.valid_until < date('now') THEN 'expired'
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

-- Rows are only ever inserted, so the index never has to retire a row.
CREATE VIRTUAL TABLE records_fts USING fts5(title, text, body, content = 'records', content_rowid = 'rowid', tokenize = 'porter unicode61');
CREATE TRIGGER records_fts_insert AFTER INSERT ON records
BEGIN INSERT INTO records_fts (rowid, title, text, body) VALUES (NEW.rowid, NEW.title, NEW.text, NEW.body); END;
