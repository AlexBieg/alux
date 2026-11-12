package records

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/glob"
	"github.com/alexbieg/alux/internal/rulecel"
)

var (
	triggerKeys = []string{"agent", "always", "bash", "path", "repo", "tool", "topic"}
	// TriggerAgents are the subagent names a trigger's agent key may give.
	TriggerAgents = []string{"coder", "curator", "migrator", "navigator", "pair", "research", "review", "verifier", "walkthrough"}
)

// A trigger is a list of clauses: the rule applies when any clause matches, and a clause matches when all its keys do.
func coerceTrigger(tx *sql.Tx, name string, v any) (any, error) {
	clauses, ok := v.([]any)
	if !ok || len(clauses) == 0 {
		return nil, refuse(`attrs.%s must be a non-empty list of clauses, like [{"repo": "alux", "path": "internal/**/*.go"}, {"tool": "grep"}]`, name)
	}
	out := make([]any, 0, len(clauses))
	for i, c := range clauses {
		at := fmt.Sprintf("attrs.%s[%d]", name, i)
		clause, ok := c.(map[string]any)
		if !ok || len(clause) == 0 {
			return nil, refuse("%s must be a non-empty object of trigger keys: %s", at, strings.Join(triggerKeys, ", "))
		}
		_, always := clause["always"]
		_, topic := clause["topic"]
		if (always || topic) && len(clause) > 1 {
			return nil, refuse("%s: always and topic stand alone in a clause; put other keys in a clause of their own", at)
		}
		norm := map[string]any{}
		for key, val := range clause {
			if !slices.Contains(triggerKeys, key) {
				return nil, refuse("%s has unknown key %q: trigger keys are %s", at, key, strings.Join(triggerKeys, ", "))
			}
			if key == "always" {
				if b, ok := val.(bool); !ok || !b {
					return nil, refuse("%s.always must be true", at)
				}
				norm[key] = true
				continue
			}
			str, ok := val.(string)
			if !ok || strings.TrimSpace(str) == "" {
				return nil, refuse("%s.%s must be a non-empty string", at, key)
			}
			switch key {
			case "agent":
				if !slices.Contains(TriggerAgents, str) {
					return nil, refuse("%s.agent must be one of %s", at, strings.Join(TriggerAgents, ", "))
				}
			case "path":
				if !glob.Valid(str) {
					return nil, refuse("%s.path is not a valid glob: each /-separated segment must be a valid pattern, and ** matches any number of folders", at)
				}
				if strings.HasPrefix(str, "/") || strings.HasPrefix(str, "~") || strings.HasPrefix(str, "./") {
					return nil, refuse("%s.path %q must be relative to the repo root, like web/**/*.tsx; pair it with repo to name the repo", at, str)
				}
			case "repo":
				id, err := resolveEntity(tx, str)
				if err != nil {
					return nil, err
				}
				if id == "" {
					return nil, refuse("%s.repo names unknown entity %q", at, str)
				}
				var typ string
				if err := tx.QueryRow(`SELECT type FROM entities WHERE id = ?`, id).Scan(&typ); err != nil {
					return nil, err
				}
				if typ != "repo" {
					return nil, refuse("%s.repo %q resolves to a %s entity, not a repo; name a repo entity (by name, or by its root path)", at, str, typ)
				}
				str = id
			}
			norm[key] = str
		}
		out = append(out, norm)
	}
	return out, nil
}

func coerceCondition(name string, v any) (any, error) {
	str, ok := v.(string)
	if !ok || strings.TrimSpace(str) == "" {
		return nil, refuse("attrs.%s must be a non-empty CEL expression", name)
	}
	if _, err := rulecel.Compile(str); err != nil {
		return nil, refuse("attrs.%s does not compile to a bool: %v", name, err)
	}
	return str, nil
}
