// Package reviewers picks the review personas for a pull request from the files it changes.
package reviewers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/alexbieg/alux/internal/glob"
	"github.com/alexbieg/alux/internal/skills"
)

// Skill is the skill a review runs, and the folder beside it that holds the personas file.
const Skill = "pr-review"

type Persona struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Always     bool     `json:"always"`
	UnlessOnly []string `json:"unless_only"`
	Paths      []string `json:"paths"`
	Lens       string   `json:"lens"`
	StaysOut   string   `json:"stays_out"`
}

// Corpus is the personas file; Cap bounds the personas chosen by path or added by the agent, not the always ones.
type Corpus struct {
	Cap      int       `json:"cap"`
	Personas []Persona `json:"personas"`
}

// Path is the personas file for the skill directory dir.
func Path(dir string) string { return filepath.Join(dir, Skill, "personas.yaml") }

// Load reads a personas file; the user's own file missing means the built-in one.
func Load(file string) (*Corpus, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		if dir, derr := skills.Dir(); derr == nil && file == Path(dir) {
			data, err = skills.Builtin(Skill + "/personas.yaml")
		}
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Corpus, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("personas: not valid YAML: %w", err)
	}
	normal, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("personas: %w", err)
	}
	var c Corpus
	dec := json.NewDecoder(bytes.NewReader(normal))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("personas: %w", err)
	}
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("personas: %w", err)
	}
	return &c, nil
}

func (c *Corpus) check() error {
	var problems []string
	if c.Cap < 1 {
		problems = append(problems, "cap must be at least 1")
	}
	seen := map[string]bool{}
	for i, p := range c.Personas {
		at := fmt.Sprintf("personas[%d]", i)
		switch {
		case p.ID == "":
			problems = append(problems, at+": no id")
		case seen[p.ID]:
			problems = append(problems, at+": id "+p.ID+" is used twice")
		}
		seen[p.ID] = true
		if p.Name == "" || p.Lens == "" {
			problems = append(problems, at+": name and lens are required")
		}
		for _, g := range slices.Concat(p.Paths, p.UnlessOnly) {
			if !glob.Valid(g) {
				problems = append(problems, at+": bad glob "+g)
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Selection is who reviews: the always personas, then path matches up to the cap, and how many slots stay open.
type Selection struct {
	Always  []Persona
	Matched []Persona
	Open    int
}

func (c *Corpus) Select(files []string) Selection {
	var s Selection
	for _, p := range c.Personas {
		if p.Always {
			if len(files) > 0 && all(files, p.UnlessOnly) {
				continue
			}
			s.Always = append(s.Always, p)
			continue
		}
		if len(s.Matched) < c.Cap && anyFile(files, p.Paths) {
			s.Matched = append(s.Matched, p)
		}
	}
	s.Open = c.Cap - len(s.Matched)
	return s
}

// Inputs are the pr-review skill's inputs for this selection.
func (s Selection) Inputs(pr string) map[string]any {
	var ids []string
	for _, p := range slices.Concat(s.Always, s.Matched) {
		ids = append(ids, p.ID)
	}
	listed := strings.Join(ids, ", ")
	if listed == "" {
		listed = "none; choose at least one"
	}
	return map[string]any{"pr": pr, "reviewers": listed, "reviewer_count": max(1, len(ids)), "open_slots": s.Open}
}

func all(files, globs []string) bool {
	if len(globs) == 0 {
		return false
	}
	for _, f := range files {
		if !anyGlob(globs, f) {
			return false
		}
	}
	return true
}

func anyFile(files, globs []string) bool {
	for _, f := range files {
		if anyGlob(globs, f) {
			return true
		}
	}
	return false
}

func anyGlob(globs []string, file string) bool {
	for _, g := range globs {
		if glob.Match(g, file) {
			return true
		}
	}
	return false
}
