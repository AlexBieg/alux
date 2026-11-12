-- Temporary ledger for moving the markdown vault into records. Mutable on purpose; drop it once nothing is pending.
CREATE TABLE vault_migration (
  path        TEXT PRIMARY KEY,
  status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'in_progress', 'done', 'skipped', 'failed')),
  claim       TEXT,
  claimed_by  TEXT,
  claimed_at  TEXT,
  finished_at TEXT,
  records     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(records) AND json_type(records) = 'array'),
  note        TEXT,
  CHECK ((status = 'in_progress') = (claim IS NOT NULL AND claimed_at IS NOT NULL)),
  CHECK (status != 'done' OR json_array_length(records) > 0),
  CHECK (status NOT IN ('skipped', 'failed') OR length(trim(coalesce(note, ''))) > 0)
) STRICT;

CREATE INDEX vault_migration_status ON vault_migration(status, path);
