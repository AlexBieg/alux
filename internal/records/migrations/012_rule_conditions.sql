-- A hold rule may carry a CEL condition, so it holds a matching call only while the condition is true.
CREATE TEMP TABLE kind_fields_copy AS SELECT kind, field, type, required, enum_values FROM kind_fields;
DROP TABLE kind_fields;
CREATE TABLE kind_fields (
  kind        TEXT NOT NULL,
  field       TEXT NOT NULL,
  type        TEXT NOT NULL CHECK (type IN ('text', 'date', 'number', 'entity', 'enum', 'list', 'trigger', 'condition')),
  required    INTEGER NOT NULL DEFAULT 0 CHECK (required IN (0, 1)),
  enum_values TEXT CHECK (enum_values IS NULL OR json_valid(enum_values)),
  PRIMARY KEY (kind, field)
) STRICT;
INSERT INTO kind_fields (kind, field, type, required, enum_values) SELECT kind, field, type, required, enum_values FROM kind_fields_copy;
DROP TABLE kind_fields_copy;

INSERT INTO kind_fields (kind, field, type, required, enum_values) VALUES
  ('rule', 'when', 'condition', 0, NULL);
