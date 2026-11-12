package records

import (
	"database/sql"
	"errors"
	"strings"
)

// Label names a record-store id, false when unknown or phi; it never sets the taint latch.
func (s *Store) Label(id string) (string, bool, error) {
	var q string
	switch {
	case strings.HasPrefix(id, "ent_"):
		q = `SELECT e.name, e.sensitivity = 'phi' FROM entity_head h JOIN entities e ON e.id = h.head WHERE h.id = ?`
	case strings.HasPrefix(id, "rec_"):
		q = `SELECT e.name || ': ' || r.title, r.sensitivity = 'phi' OR e.sensitivity = 'phi'
			FROM records r JOIN entity_head h ON h.id = r.subject_id JOIN entities e ON e.id = h.head WHERE r.id = ?`
	case strings.HasPrefix(id, "src_"):
		q = `SELECT coalesce(nullif(s.title, ''), s.type || ': ' || s.ref), EXISTS (
				SELECT 1 FROM records r JOIN entity_head h ON h.id = r.subject_id JOIN entities e ON e.id = h.head
				WHERE (r.derived_from = s.id OR r.id IN (SELECT record_id FROM record_sources WHERE source_id = s.id))
					AND (r.sensitivity = 'phi' OR e.sensitivity = 'phi'))
			FROM sources s WHERE s.id = ?`
	default:
		return "", false, nil
	}
	return s.label(q, id)
}

// TicketLabel names a ticket key from the entity whose name or alias it is, false when none or phi.
func (s *Store) TicketLabel(key string) (string, bool, error) {
	var name, description string
	var phi bool
	err := s.db.QueryRow(`SELECT e.name, coalesce(e.description, ''), e.sensitivity = 'phi' FROM entities e WHERE e.id = (
			SELECT h.head FROM entities x JOIN entity_head h ON h.id = x.id WHERE x.name = ?1
			UNION ALL
			SELECT h.head FROM entity_aliases a JOIN entity_head h ON h.id = a.entity_id WHERE a.alias = ?1
			LIMIT 1)`, key).Scan(&name, &description, &phi)
	if errors.Is(err, sql.ErrNoRows) || phi {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	label := strings.TrimLeft(trimKey(name, key), " :-–—")
	if label == "" {
		label = description
	}
	return label, label != "", nil
}

func trimKey(s, key string) string {
	if len(s) >= len(key) && strings.EqualFold(s[:len(key)], key) {
		return s[len(key):]
	}
	return s
}

func (s *Store) label(q, id string) (string, bool, error) {
	var name string
	var phi bool
	err := s.db.QueryRow(q, id).Scan(&name, &phi)
	if errors.Is(err, sql.ErrNoRows) || phi {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, name != "", nil
}
