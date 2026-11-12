package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/paths"
)

func TestPromptVersionFollowsTheTemplateAndStartupRulesButNotTheSession(t *testing.T) {
	s, repo := ruleStore(t)
	addRule(t, s, "the always rule", map[string]any{"always": true})
	first := injector(s, repo)
	v := PromptVersion(first)
	if again := PromptVersion(injector(s, repo)); again != v {
		t.Errorf("a second session under the same rules got %s, want %s", again, v)
	}
	if edited := promptVersion("an edited template", first.startupIDs()); edited == v {
		t.Errorf("an edited template kept version %s", v)
	}
	addRule(t, s, "a second always rule", map[string]any{"always": true})
	if added := PromptVersion(injector(s, repo)); added == v {
		t.Errorf("a new startup rule kept version %s", v)
	}
}

// excludeRepo makes a repo under a fresh HOME holding files, with settings.json set to settings.
func excludeRepo(t *testing.T, settings string, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "Code", "repo")
	files[filepath.Join(home, paths.Config, "settings.json")] = settings
	for name, body := range files {
		path := name
		if !filepath.IsAbs(name) {
			path = filepath.Join(repo, name)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

const agentsMD = `# Repo

Intro line.

## Comments

Write long comments.

### Comment style

Use banners.

## Testing

Run go test.
### Fixtures

Keep them small.`

func TestAnExcludedSectionGoesWithItsSubsectionsAndTheRestStaysVerbatim(t *testing.T) {
	repo := excludeRepo(t, `{"agents_md_exclude": {"~/Code/repo": [" comments "]}}`,
		map[string]string{"AGENTS.md": agentsMD})
	prompt, _, notices := projectPrompt(repo, repo, repo)
	for _, gone := range []string{"## Comments", "Write long comments.", "### Comment style", "Use banners."} {
		if strings.Contains(prompt, gone) {
			t.Errorf("excluded text %q is still in the prompt:\n%s", gone, prompt)
		}
	}
	want := "# Repo\n\nIntro line.\n\n## Testing\n\nRun go test.\n### Fixtures\n\nKeep them small."
	if !strings.Contains(prompt, want) {
		t.Errorf("the sections around the excluded one changed; want\n%s\nin\n%s", want, prompt)
	}
	if len(notices) != 0 {
		t.Errorf("a heading that is present raised notices: %q", notices)
	}
}

func TestAHeadingInsideAFenceIsNotASection(t *testing.T) {
	body := "# Repo\n\n## Setup\n\n```md\n## Comments\nnot a section\n```\n\nAfter the fence.\n\n## Comments\n\nDrop me."
	repo := excludeRepo(t, `{"agents_md_exclude": {"~/Code/repo": ["Comments"]}}`,
		map[string]string{"AGENTS.md": body})
	prompt, _, _ := projectPrompt(repo, repo, repo)
	want := "## Setup\n\n```md\n## Comments\nnot a section\n```\n\nAfter the fence."
	if !strings.Contains(prompt, want) {
		t.Errorf("the fenced heading was taken for a section; want\n%s\nin\n%s", want, prompt)
	}
	if strings.Contains(prompt, "Drop me.") {
		t.Errorf("the real Comments section after the fence survived:\n%s", prompt)
	}
}

func TestTheExclusionAppliesToClaudeMDAndNestedFiles(t *testing.T) {
	repo := excludeRepo(t, `{"agents_md_exclude": {"~/Code/repo": ["Comments"]}}`, map[string]string{
		"CLAUDE.md":     "# Claude\n\n## Comments\n\nFrom CLAUDE.md.\n\n## Kept\n\nClaude kept.",
		"pkg/AGENTS.md": "## Comments\n\nFrom nested AGENTS.md.\n\n## Nested\n\nNested kept.",
	})
	prompt, files, _ := projectPrompt(filepath.Join(repo, "pkg"), repo, repo)
	if strings.Contains(prompt, "From CLAUDE.md.") || strings.Contains(prompt, "From nested AGENTS.md.") {
		t.Errorf("an excluded section survived in CLAUDE.md or a nested file:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Claude kept.") || !strings.Contains(prompt, "Nested kept.") {
		t.Errorf("sections not on the list went missing:\n%s", prompt)
	}
	if len(files) != 2 {
		t.Errorf("want both files loaded, got %q", files)
	}
}

func TestAListedHeadingNoFileHasRaisesOneNotice(t *testing.T) {
	repo := excludeRepo(t, `{"agents_md_exclude": {"~/Code/repo": ["Comments", "Starting the Stack"]}}`,
		map[string]string{"AGENTS.md": agentsMD})
	_, _, notices := projectPrompt(repo, repo, repo)
	want := `AGENTS.md exclude: no section "Starting the Stack" in ` + repo + " — it may have been renamed"
	if len(notices) != 1 || notices[0] != want {
		t.Errorf("notices = %q, want only %q", notices, want)
	}
}

func TestARepoWithNoEntryGetsItsFileUnchanged(t *testing.T) {
	repo := excludeRepo(t, `{"agents_md_exclude": {"/somewhere/else": ["Comments"]}}`,
		map[string]string{"AGENTS.md": agentsMD})
	prompt, _, notices := projectPrompt(repo, repo, repo)
	want := "<project-instructions path=\"" + filepath.Join(repo, "AGENTS.md") + "\">\n" + agentsMD + "\n</project-instructions>"
	if prompt != want {
		t.Errorf("prompt = \n%s\nwant\n%s", prompt, want)
	}
	if len(notices) != 0 {
		t.Errorf("another repo's list raised notices here: %q", notices)
	}
}
