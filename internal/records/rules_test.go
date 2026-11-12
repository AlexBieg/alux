package records

import (
	"slices"
	"testing"
)

func TestClauseMatchesACallOnlyWhenEveryKeyItSetsDoes(t *testing.T) {
	edit := Call{Tool: "str_replace_based_edit_tool", Repo: "ent_web", Path: "apps/a/Table.tsx"}
	push := Call{Tool: "bash", Repo: "ent_web", Commands: []string{"cd web && jj git push", "cd web", "jj git push"}}
	for _, c := range []struct {
		name   string
		clause Clause
		call   Call
		want   bool
	}{
		{"tool", Clause{Tool: "str_replace_based_edit_tool"}, edit, true},
		{"other tool", Clause{Tool: "grep"}, edit, false},
		{"repo and path", Clause{Repo: "ent_web", Path: "apps/**/*.tsx"}, edit, true},
		{"path in another repo", Clause{Repo: "ent_other", Path: "apps/**/*.tsx"}, edit, false},
		{"path that does not match", Clause{Path: "api/**/*.py"}, edit, false},
		{"path on a call with no file", Clause{Path: "**"}, push, false},
		{"bash prefix of one command in the line", Clause{Bash: "jj git push"}, push, true},
		{"bash prefix of the whole line", Clause{Bash: "cd web && jj"}, push, true},
		{"bash prefix stops at a word", Clause{Bash: "jj git pu"}, push, false},
		{"bash on a call that runs nothing", Clause{Bash: "jj"}, edit, false},
		{"agent clause outside its agent", Clause{Agent: "coder", Tool: "str_replace_based_edit_tool"}, edit, false},
		{"agent clause inside its agent", Clause{Agent: "coder", Tool: "str_replace_based_edit_tool"}, Call{Agent: "coder", Tool: edit.Tool}, true},
		{"bare agent never fires on a call", Clause{Agent: "coder"}, Call{Agent: "coder", Tool: "grep"}, false},
		{"repo alone fires on a file in it", Clause{Repo: "ent_web"}, edit, true},
		{"repo alone does not fire on a fileless call", Clause{Repo: "ent_web"}, push, false},
		{"topic", Clause{Topic: "planning"}, edit, false},
		{"always", Clause{Always: true}, edit, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.clause.Matches(c.call); got != c.want {
				t.Fatalf("Matches = %v, want %v", got, c.want)
			}
		})
	}
}

func TestABashClauseFiresPastGlobalFlagsAndThroughAScriptRunner(t *testing.T) {
	for _, c := range []struct {
		prefix, command string
		want            bool
	}{
		{"jj git push", "jj --no-pager git push --bookmark x", true},
		{"jj git push", "jj -R ../other git push", true},
		{"jj git push", "jj --repository=../other git push", true},
		{"jj git push", "/opt/homebrew/bin/jj git push", true},
		{"jj rebase", "jj --at-op abc rebase -d main", true},
		{"gh pr create", "gh -R acme/app pr create --draft", true},
		{"gh release create", "gh -R acme/app release create v1", true},
		{"scripts/bootstrap.sh", "bash /Users/someone/Code/acme/scripts/bootstrap.sh", true},
		{"./scripts/bootstrap.sh", ". scripts/bootstrap.sh", true},
		{"source scripts/bootstrap.sh", "sh ./scripts/bootstrap.sh", true},
		{"jj git push", "jj -R push git fetch", false},
		{"jj git push", "jj log -r push", false},
		{"gh pr create", "gh pr list", false},
		{"scripts/bootstrap.sh", "bash -c 'echo hi'", false},
	} {
		t.Run(c.command, func(t *testing.T) {
			call := Call{Tool: "bash", Commands: []string{c.command}}
			if got := (Clause{Bash: c.prefix}).Matches(call); got != c.want {
				t.Fatalf("%q on %q = %v, want %v", c.prefix, c.command, got, c.want)
			}
		})
	}
}

func TestClauseLoadsAtStartForAlwaysAndForMatchingRepoAndAgentKeysOnly(t *testing.T) {
	for _, c := range []struct {
		name   string
		clause Clause
		agent  string
		want   bool
	}{
		{"always", Clause{Always: true}, "", true},
		{"this repo", Clause{Repo: "ent_web"}, "", true},
		{"another repo", Clause{Repo: "ent_other"}, "", false},
		{"bare agent, that agent", Clause{Agent: "coder"}, "coder", true},
		{"bare agent, main agent", Clause{Agent: "coder"}, "", false},
		{"agent in this repo", Clause{Agent: "coder", Repo: "ent_web"}, "coder", true},
		{"tool clause waits for its call", Clause{Repo: "ent_web", Tool: "grep"}, "", false},
		{"topic", Clause{Topic: "planning"}, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.clause.AtStart(c.agent, "ent_web"); got != c.want {
				t.Fatalf("AtStart = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRepoIDFindsARepoNamedByItsPathUnderHome(t *testing.T) {
	t.Setenv("HOME", "/Users/someone")
	s := open(t)
	dotfiles := entity(t, s, EntityInput{Type: "repo", Name: "Dotfiles Repo", Aliases: []string{"~/Code/dotfiles"}, Sensitivity: "normal"})
	if id, err := s.RepoID("/Users/someone/Code/dotfiles"); err != nil || id != dotfiles.ID {
		t.Fatalf("RepoID = %q, %v; want %s", id, err, dotfiles.ID)
	}
}

func TestRulesListsCurrentRulesWithTriggerReposFollowingAMerge(t *testing.T) {
	s := open(t)
	old := entity(t, s, EntityInput{Type: "repo", Name: "acme-old", Sensitivity: "normal"})
	head := entity(t, s, EntityInput{Type: "repo", Name: "acme/app", Aliases: []string{"/Users/someone/Code/acme"}, Sensitivity: "normal"})
	trigger := map[string]any{"how_to_apply": "Use the defaults.", "mode": "advise",
		"trigger": []any{map[string]any{"repo": "acme-old", "path": "web/**"}}}
	kept := write(t, s, rec("rule", "Use Helix defaults", trigger))
	replaced := write(t, s, rec("rule", "Old wording of a rule", map[string]any{"trigger": []any{map[string]any{"tool": "grep"}}}))
	replacement := rec("rule", "New wording of a rule", map[string]any{"trigger": []any{map[string]any{"tool": "grep"}}})
	replacement.Supersedes = IDs{replaced.ID}
	current := write(t, s, replacement)
	gone := write(t, s, rec("rule", "A rule that was never true", nil))
	if err := s.Event(gone.ID, "retracted", "wrong", agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditEntity(EntityEdit{Name: "acme-old", MergeInto: "acme/app"}, "same repo", agent); err != nil {
		t.Fatal(err)
	}

	rules, err := s.Rules()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rules {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{kept.ID, current.ID}) {
		t.Fatalf("rules = %v, want only the current ones %v", ids, []string{kept.ID, current.ID})
	}
	if got := rules[0].Trigger[0].Repo; got != head.ID || got == old.ID {
		t.Fatalf("trigger repo = %s, want the merge head %s", got, head.ID)
	}
	if rules[0].HowToApply != "Use the defaults." || rules[0].Mode != "advise" {
		t.Fatalf("rule fields = %+v", rules[0])
	}
	if id, err := s.RepoID("/Users/someone/Code/acme"); err != nil || id != head.ID {
		t.Fatalf("RepoID by root path = %q, %v; want %s", id, err, head.ID)
	}
	if id, _ := s.RepoID("subagents"); id != "" {
		t.Fatalf("RepoID named a system entity as a repo: %s", id)
	}
}
