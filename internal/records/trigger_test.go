package records

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuleTriggerIsStoredWithItsRepoResolved(t *testing.T) {
	s := open(t)
	repo := entity(t, s, EntityInput{Type: "repo", Name: "acme", Sensitivity: "normal"})
	res := write(t, s, rec("rule", "Use Helix defaults", map[string]any{
		"mode": "advise",
		"trigger": []any{
			map[string]any{"repo": "acme", "path": "web/**/*.tsx"},
			map[string]any{"tool": "grep"},
			map[string]any{"topic": "interviews"},
			map[string]any{"agent": "coder"},
		},
	}))
	var raw string
	if err := s.db.QueryRow(`SELECT json_extract(attrs, '$.trigger') FROM records WHERE id = ?`, res.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0]["repo"] != repo.ID || got[0]["path"] != "web/**/*.tsx" || got[1]["tool"] != "grep" || got[2]["topic"] != "interviews" || got[3]["agent"] != "coder" {
		t.Fatalf("stored trigger %s", raw)
	}
	if strings.Contains(raw, "extra") || len(res.Notes) != 0 {
		t.Fatalf("trigger or mode went under extra: %s %v", raw, res.Notes)
	}
}

func TestAHoldRuleConditionIsStoredAndReadBackByRules(t *testing.T) {
	s := open(t)
	when := `call.tool == 'pr_create' && words(call.args.body) > 400`
	res := write(t, s, rec("rule", "Keep PR bodies short", map[string]any{
		"mode": "hold", "when": when, "trigger": []any{map[string]any{"tool": "pr_create"}},
	}))
	if len(res.Notes) != 0 {
		t.Fatalf("when went under extra: %v", res.Notes)
	}
	rules, err := s.Rules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].When != when {
		t.Fatalf("rules %+v, want one with when %q", rules, when)
	}
}

func TestAConditionThatDoesNotCompileOrSitsOnANonHoldRuleIsRefused(t *testing.T) {
	s := open(t)
	trigger := []any{map[string]any{"tool": "pr_create"}}
	for _, c := range []struct {
		name, want string
		attrs      map[string]any
	}{
		{"syntax error", "attrs.when does not compile to a bool: ERROR", map[string]any{"mode": "hold", "when": "call.tool ==", "trigger": trigger}},
		{"unknown function", "undeclared reference to 'nope'", map[string]any{"mode": "hold", "when": "nope(call.path)", "trigger": trigger}},
		{"not a bool", "not a bool", map[string]any{"mode": "hold", "when": "words(call.args.body)", "trigger": trigger}},
		{"blank", "attrs.when must be a non-empty CEL expression", map[string]any{"mode": "hold", "when": " ", "trigger": trigger}},
		{"advise rule", `attrs.when applies only to a rule with mode "hold"`, map[string]any{"mode": "advise", "when": "true", "trigger": trigger}},
		{"no mode", `attrs.when applies only to a rule with mode "hold"`, map[string]any{"when": "true", "trigger": trigger}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Add(rec("rule", "Some rule", c.attrs), agent)
			rejected(t, err, c.want)
		})
	}
	if n := count(t, s, `SELECT count(*) FROM records`); n != 0 {
		t.Fatalf("%d records written by refused adds", n)
	}
}

func TestTriggerRepoResolvesARepoEntityByItsRootPath(t *testing.T) {
	s := open(t)
	repo := entity(t, s, EntityInput{Type: "repo", Name: "acme/app", Aliases: []string{"/Users/someone/Code/acme"}, Sensitivity: "normal"})
	res := write(t, s, rec("rule", "Use Helix defaults", map[string]any{
		"trigger": []any{map[string]any{"repo": "/Users/someone/Code/acme", "path": "web/**/*.tsx"}},
	}))
	var got string
	if err := s.db.QueryRow(`SELECT json_extract(attrs, '$.trigger[0].repo') FROM records WHERE id = ?`, res.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != repo.ID {
		t.Fatalf("repo stored as %q, want %s", got, repo.ID)
	}
}

func TestMalformedRuleTriggersAreRefused(t *testing.T) {
	s := open(t)
	entity(t, s, EntityInput{Type: "system", Name: "harness", Aliases: []string{"alux"}, Sensitivity: "normal"})
	clause := func(c map[string]any) map[string]any { return map[string]any{"trigger": []any{c}} }
	for _, c := range []struct {
		name, want string
		attrs      map[string]any
	}{
		{"not a list", "attrs.trigger must be a non-empty list of clauses", map[string]any{"trigger": "grep"}},
		{"empty list", "attrs.trigger must be a non-empty list of clauses", map[string]any{"trigger": []any{}}},
		{"empty clause", "attrs.trigger[0] must be a non-empty object", clause(map[string]any{})},
		{"unknown key", `unknown key "file"`, clause(map[string]any{"file": "*.go"})},
		{"unknown agent", "attrs.trigger[0].agent must be one of", clause(map[string]any{"agent": "coders"})},
		{"agent with topic", "always and topic stand alone", clause(map[string]any{"agent": "coder", "topic": "interviews"})},
		{"event key", `unknown key "event"`, clause(map[string]any{"event": "push"})},
		{"bad glob", "attrs.trigger[0].path is not a valid glob", clause(map[string]any{"path": "web/[*.tsx"})},
		{"absolute path", "must be relative to the repo root", clause(map[string]any{"path": "/Users/someone/Code/alux/internal/ui/*.go"})},
		{"home path", "must be relative to the repo root", clause(map[string]any{"path": "~/.config/**"})},
		{"unknown repo", `attrs.trigger[0].repo names unknown entity "nowhere"`, clause(map[string]any{"repo": "nowhere"})},
		{"repo is not a repo", `attrs.trigger[0].repo "alux" resolves to a system entity, not a repo`, clause(map[string]any{"repo": "alux"})},
		{"always false", "attrs.trigger[0].always must be true", clause(map[string]any{"always": false})},
		{"topic with others", "always and topic stand alone", clause(map[string]any{"topic": "interviews", "tool": "grep"})},
		{"blank tool", "attrs.trigger[0].tool must be a non-empty string", clause(map[string]any{"tool": " "})},
		{"bad mode", "attrs.mode must be one of hold, advise", map[string]any{"mode": "block"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Add(rec("rule", "Some rule", c.attrs), agent)
			rejected(t, err, c.want)
		})
	}
	if n := count(t, s, `SELECT count(*) FROM records`); n != 0 {
		t.Fatalf("%d records written by refused adds", n)
	}
}
