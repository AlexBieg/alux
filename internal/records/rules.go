package records

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/glob"
	"github.com/alexbieg/alux/internal/rulecel"
)

// Rule is a current rule record in the shape code applies it.
type Rule struct {
	ID, Subject, Title, Text, HowToApply, Mode string
	Trigger                                    []Clause
	When                                       string
}

// Clause is one trigger clause; its set keys must all match.
type Clause struct {
	Agent  string `json:"agent"`
	Always bool   `json:"always"`
	Bash   string `json:"bash"`
	Path   string `json:"path"`
	Repo   string `json:"repo"`
	Tool   string `json:"tool"`
	Topic  string `json:"topic"`
}

// Call is what a trigger is matched against: one tool call, the agent making it, and the repo it touches.
type Call struct {
	// Agent is the subagent's name, "" for the main agent.
	Agent string
	Tool  string
	// Commands are the shell commands the call runs, each as one line.
	Commands []string
	// Repo is the entity id of the repo holding Path, or of the session's repo when the call has no path.
	Repo string
	// Path is the file read or edited, relative to its repo's root, or absolute without the leading slash outside any repo.
	Path string
}

// Rules is every current rule that is not PHI, with trigger repos resolved to the entity they now answer to.
func (s *Store) Rules() ([]Rule, error) {
	rows, err := s.rows(fmt.Sprintf(`SELECT %s FROM record_status r %s
		WHERE r.kind = 'rule' AND r.status = 'active' AND r.sensitivity = 'normal' AND e.sensitivity = 'normal'
		ORDER BY r.rid`, rowColumns, subjectJoin))
	if err != nil {
		return nil, err
	}
	heads := map[string]string{}
	out := make([]Rule, 0, len(rows))
	for _, row := range rows {
		var attrs struct {
			HowToApply string   `json:"how_to_apply"`
			Mode       string   `json:"mode"`
			Trigger    []Clause `json:"trigger"`
			When       string   `json:"when"`
		}
		if err := json.Unmarshal([]byte(row.Attrs), &attrs); err != nil {
			return nil, fmt.Errorf("rule %s: %w", row.ID, err)
		}
		for i, c := range attrs.Trigger {
			if c.Repo == "" {
				continue
			}
			head, ok := heads[c.Repo]
			if !ok {
				if head, err = resolveEntity(s.db, c.Repo); err != nil {
					return nil, err
				}
				heads[c.Repo] = head
			}
			attrs.Trigger[i].Repo = head
		}
		out = append(out, Rule{ID: row.ID, Subject: row.Subject, Title: row.Title, Text: row.Text,
			HowToApply: attrs.HowToApply, Mode: attrs.Mode, Trigger: attrs.Trigger, When: attrs.When})
	}
	return out, nil
}

// TopicDescriptions maps each lower-cased name and alias of a topic entity that is not PHI to its description.
func (s *Store) TopicDescriptions() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT x.name, e.description FROM entities x JOIN entity_head h ON h.id = x.id JOIN entities e ON e.id = h.head
			WHERE e.type = 'topic' AND e.sensitivity = 'normal' AND coalesce(e.description, '') <> ''
		UNION ALL SELECT a.alias, e.description FROM entity_aliases a JOIN entity_head h ON h.id = a.entity_id JOIN entities e ON e.id = h.head
			WHERE e.type = 'topic' AND e.sensitivity = 'normal' AND coalesce(e.description, '') <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, description string
		if err := rows.Scan(&name, &description); err != nil {
			return nil, err
		}
		out[strings.ToLower(strings.TrimSpace(name))] = strings.Join(strings.Fields(description), " ")
	}
	return out, rows.Err()
}

// RepoID is the repo entity a root path names, as written or under ~, or "" when none does.
func (s *Store) RepoID(root string) (string, error) {
	id, err := resolveEntity(s.db, root)
	if err != nil {
		return "", err
	}
	if home, herr := os.UserHomeDir(); id == "" && herr == nil && strings.HasPrefix(root, home+string(filepath.Separator)) {
		if id, err = resolveEntity(s.db, "~"+strings.TrimPrefix(root, home)); err != nil {
			return "", err
		}
	}
	if id == "" {
		return "", nil
	}
	var typ string
	if err := s.db.QueryRow(`SELECT type FROM entities WHERE id = ?`, id).Scan(&typ); err != nil {
		return "", err
	}
	if typ != "repo" {
		return "", nil
	}
	return id, nil
}

// Fires reports whether any clause matches call.
func (r Rule) Fires(call Call) bool {
	for _, c := range r.Trigger {
		if c.Matches(call) {
			return true
		}
	}
	return false
}

// Topics are the topics the rule's clauses name, in clause order.
func (r Rule) Topics() []string {
	var out []string
	for _, c := range r.Trigger {
		if c.Topic != "" {
			out = append(out, c.Topic)
		}
	}
	return out
}

// AtStart reports whether any clause loads the rule when an agent starts in repo.
func (r Rule) AtStart(agent, repo string) bool {
	for _, c := range r.Trigger {
		if c.AtStart(agent, repo) {
			return true
		}
	}
	return false
}

func (c Clause) callKeys() bool { return c.Tool != "" || c.Bash != "" || c.Path != "" }

// AtStart is true for always, and for a clause of only repo and agent keys that both match.
func (c Clause) AtStart(agent, repo string) bool {
	switch {
	case c.Always:
		return true
	case c.Topic != "" || c.callKeys():
		return false
	case c.Repo == "" && c.Agent == "":
		return false
	}
	return (c.Repo == "" || c.Repo == repo) && (c.Agent == "" || c.Agent == agent)
}

// Matches is true when every key the clause sets matches the call; a clause of only repo and agent keys matches a file in that repo.
func (c Clause) Matches(call Call) bool {
	if c.Always || c.Topic != "" {
		return false
	}
	if c.Agent != "" && c.Agent != call.Agent {
		return false
	}
	if !c.callKeys() {
		return c.Repo != "" && call.Path != "" && call.Repo == c.Repo
	}
	if c.Repo != "" && c.Repo != call.Repo {
		return false
	}
	if c.Tool != "" && c.Tool != call.Tool {
		return false
	}
	if c.Path != "" && (call.Path == "" || !glob.Match(c.Path, call.Path)) {
		return false
	}
	if c.Bash != "" && !anyPrefix(call.Commands, c.Bash) {
		return false
	}
	return true
}

// anyPrefix matches whole words, so "jj new" does not fire on "jj newt", against each command as written and as normalised.
func anyPrefix(commands []string, prefix string) bool {
	return slices.ContainsFunc(commands, func(cmd string) bool { return rulecel.Cmd(cmd, prefix) })
}
