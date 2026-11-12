// Package skills loads and validates the YAML skills built into alux and those kept in ~/.config/alux/skills.
package skills

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/google/jsonschema-go/jsonschema"
	"go.yaml.in/yaml/v4"
)

const Format = 2

//go:embed schema/v2.json
var schemaV2 []byte

//go:embed schema/flow.json
var schemaFlow []byte

//go:embed builtin
var builtinFS embed.FS

// Builtin is a file under internal/skills/builtin, such as "pr-review/personas.yaml".
func Builtin(name string) ([]byte, error) { return builtinFS.ReadFile("builtin/" + name) }

// inUserDir reports whether dir is the user's skills folder, the only one built-ins stand behind.
func inUserDir(dir string) bool {
	user, err := Dir()
	return err == nil && filepath.Clean(dir) == user
}

type Skill struct {
	Schema   int               `json:"schema"`
	Name     string            `json:"skill"`
	Version  int               `json:"version"`
	When     string            `json:"when"`
	NotFor   string            `json:"not_for"`
	Starts   []string          `json:"starts"`
	Requires Requires          `json:"requires"`
	Inputs   map[string]Input  `json:"inputs"`
	Vars     map[string]string `json:"vars"`
	Nodes    map[string]Node   `json:"nodes"`
	Order    [][]string        `json:"order"`
	Stages   []Stage           `json:"stages"`
}

// Flow reports whether the file is a flow of stages rather than a checklist of nodes.
func (s *Skill) Flow() bool { return len(s.Stages) > 0 }

type Requires struct {
	Tools         []string `json:"tools"`
	OptionalTools []string `json:"optional_tools"`
}

type Input struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Default     any    `json:"default"`
	Enum        []any  `json:"enum"`
}

type Node struct {
	Say      string     `json:"say"`
	Optional bool       `json:"optional"`
	Strict   bool       `json:"strict"`
	When     *Condition `json:"when"`
	Loop     *Loop      `json:"loop"`
	Evidence []Rule     `json:"evidence"`
}

// Step is a run stage's one tool call; Capture names values to take from its output for later stages' inputs.
type Step struct {
	Tool    string             `json:"tool"`
	Input   map[string]any     `json:"input"`
	Capture map[string]Capture `json:"capture"`
}

// Capture is the first group of Pattern in a step's output; an int fills a field that is exactly its ${name} as a number.
type Capture struct {
	Pattern string `json:"pattern"`
	Type    string `json:"type"`
}

type Loop struct {
	Times Count `json:"times"`
}

// Condition is exactly one of: a tool being available, or an input, or a field of the out finish_stage gives, equal to a value or in a list.
type Condition struct {
	Available string          `json:"available"`
	Input     string          `json:"input"`
	Out       string          `json:"out"`
	Equals    json.RawMessage `json:"equals"`
	In        []any           `json:"in"`
}

// Describe is the condition in words, such as "when web_search is available".
func (c *Condition) Describe() string {
	what := "input " + c.Input
	if c.Out != "" {
		what = "out " + c.Out
	}
	switch {
	case c.Available != "":
		return "when " + c.Available + " is available"
	case c.In != nil:
		return fmt.Sprintf("when %s is one of %v", what, c.In)
	}
	return fmt.Sprintf("when %s is %s", what, c.Equals)
}

// Rule is exactly one of the evidence rule types; the schema guarantees one field is set.
type Rule struct {
	Call             *CallRule         `json:"call"`
	FetchesOpened    *FetchesOpened    `json:"fetches_opened"`
	ArtifactLines    *ArtifactLines    `json:"artifact_lines"`
	CitationsFetched *CitationsFetched `json:"citations_fetched"`
	LeadsFollowed    *LeadsFollowed    `json:"leads_followed"`
	SubagentSkill    *SubagentSkill    `json:"subagent_skill"`
	PollStopped      *PollStopped      `json:"poll_stopped"`
}

// PollStopped counts polls the model started that stopped on their own, by their condition or their time limit.
type PollStopped struct {
	Min *Count `json:"min"`
}

type SubagentSkill struct {
	Skill string `json:"skill"`
	Min   *Count `json:"min"`
}

// CallRule counts successful calls to any of Tool for which the CEL condition When, if set, holds.
type CallRule struct {
	Tool     Tools  `json:"tool"`
	When     string `json:"when"`
	Min      *Count `json:"min"`
	Distinct string `json:"distinct"`
}

// Tools is one tool name or a list of them.
type Tools []string

func (t *Tools) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*t = Tools{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(t))
}

type FetchesOpened struct {
	Min Count `json:"min"`
}

// ArtifactLines counts Path's lines matching Pattern, only under the heading Section when it is set.
type ArtifactLines struct {
	Path    string  `json:"path"`
	Pattern string  `json:"pattern"`
	Min     Count   `json:"min"`
	Section string  `json:"section"`
	Max     *Count  `json:"max"`
	Covers  *Covers `json:"covers"`
}

// Covers asks that every line of Path matching Pattern share its key, the first non-empty group, with a counted line.
type Covers struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
}

type CitationsFetched struct {
	Path string `json:"path"`
	Min  *Count `json:"min"`
}

type LeadsFollowed struct {
	Path       string `json:"path"`
	Min        Count  `json:"min"`
	NotInInput string `json:"not_in_input"`
}

// Count is a literal number or the name of an int input, written "${name}".
type Count struct {
	N   int
	Ref string
}

func (c *Count) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		c.Ref = strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}")
		return nil
	}
	return json.Unmarshal(b, &c.N)
}

// String is the count as a skill file writes it.
func (c Count) String() string {
	if c.Ref != "" {
		return "${" + c.Ref + "}"
	}
	return fmt.Sprint(c.N)
}

type Problems []string

func (p Problems) Error() string { return strings.Join(p, "; ") }

// Dir is ~/.config/alux/skills, beside settings.json.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, paths.Config, "skills"), nil
}

// Load reads and validates one skill file, whose base name must match its skill: key.
// A file missing from the user's skills folder falls back to the built-in of that name.
func Load(path string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && inUserDir(filepath.Dir(path)) {
		if built, berr := Builtin(filepath.Base(path)); berr == nil {
			data, err = built, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return Parse(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), withSettings(data))
}

const knowledgeToken = "${knowledge_tool}"

// withSettings fills ${knowledge_tool}; unset, it names a tool no session offers, so a step gated on it is skipped.
func withSettings(data []byte) []byte {
	if !bytes.Contains(data, []byte(knowledgeToken)) {
		return data
	}
	tool := KnowledgeTool()
	if tool == "" {
		tool = "knowledge_tool"
	}
	return bytes.ReplaceAll(data, []byte(knowledgeToken), []byte(tool))
}

// KnowledgeTool is settings.json's knowledge_tool, the MCP tool that searches internal knowledge, or "" when unset.
func KnowledgeTool() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return ""
	}
	var settings struct {
		KnowledgeTool string `json:"knowledge_tool"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return ""
	}
	return settings.KnowledgeTool
}

// Parse validates a skill file's bytes; name is the file's base name, or "" to skip that check.
func Parse(name string, data []byte) (*Skill, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, Problems{"not valid YAML: " + err.Error()}
	}
	top, ok := raw.(map[string]any)
	if !ok {
		return nil, Problems{"a skill file must be a YAML mapping"}
	}
	switch v, present := top["schema"]; {
	case !present:
		return nil, Problems{fmt.Sprintf("no schema: key; add schema: %d", Format)}
	case v == 1:
		return nil, Problems{"schema 1 is no longer supported: set schema: 2; step, dispatch and expand nodes are now flow stages (docs/skill-flow.md)"}
	case v != Format:
		return nil, Problems{fmt.Sprintf("schema: %v is not a format alux reads; it reads %d", v, Format)}
	}
	_, hasNodes := top["nodes"]
	_, hasStages := top["stages"]
	if hasNodes && hasStages {
		return nil, Problems{"a file has nodes: (a checklist) or stages: (a flow), not both"}
	}
	normal, err := json.Marshal(raw)
	if err != nil {
		return nil, Problems{"cannot be read as JSON: " + err.Error()}
	}
	var instance any
	if err := json.Unmarshal(normal, &instance); err != nil {
		return nil, Problems{"cannot be read as JSON: " + err.Error()}
	}
	if hasStages {
		return parseFlow(name, normal, instance)
	}
	schemas, err := v2()
	if err != nil {
		return nil, err
	}
	if problem := schemas.locate(instance); problem != "" {
		return nil, Problems{problem}
	}
	var s Skill
	dec := json.NewDecoder(bytes.NewReader(normal))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, Problems{err.Error()}
	}
	if len(s.Starts) == 0 {
		s.Starts = []string{"user", "model"}
	}
	if problems := s.check(name); len(problems) > 0 {
		return nil, problems
	}
	return &s, nil
}

// node and rule are resolved on their own so a failure can be pinned to the node and evidence item it is in.
type schemas struct {
	root, node, rule *jsonschema.Resolved
}

var (
	v2Once    sync.Once
	v2Schemas schemas
	v2Err     error
)

func v2() (schemas, error) {
	v2Once.Do(func() {
		var doc map[string]any
		if v2Err = json.Unmarshal(schemaV2, &doc); v2Err != nil {
			v2Err = fmt.Errorf("embedded skill schema: %w", v2Err)
			return
		}
		resolve := func(def string) *jsonschema.Resolved {
			if v2Err != nil {
				return nil
			}
			d := doc
			if def != "" {
				d = map[string]any{"$schema": doc["$schema"], "$defs": doc["$defs"], "$ref": "#/$defs/" + def}
			}
			data, _ := json.Marshal(d)
			var s jsonschema.Schema
			if v2Err = json.Unmarshal(data, &s); v2Err != nil {
				v2Err = fmt.Errorf("embedded skill schema %s: %w", def, v2Err)
				return nil
			}
			r, err := s.Resolve(nil)
			if err != nil {
				v2Err = fmt.Errorf("embedded skill schema %s: %w", def, err)
			}
			return r
		}
		v2Schemas = schemas{root: resolve(""), node: resolve("node"), rule: resolve("rule")}
	})
	return v2Schemas, v2Err
}

// locate validates a decoded file and describes the first failure by where it is in the file, or returns "".
func (s schemas) locate(instance any) string {
	err := s.root.Validate(instance)
	if err == nil {
		return ""
	}
	top, _ := instance.(map[string]any)
	nodes, _ := top["nodes"].(map[string]any)
	if problem := s.locateNodes("nodes", nodes); problem != "" {
		return problem
	}
	return readable("", err)
}

func (s schemas) locateNodes(at string, nodes map[string]any) string {
	for _, name := range slices.Sorted(maps.Keys(nodes)) {
		nodeErr := s.node.Validate(nodes[name])
		if nodeErr == nil {
			continue
		}
		node, _ := nodes[name].(map[string]any)
		evidence, _ := node["evidence"].([]any)
		for i, rule := range evidence {
			if ruleErr := s.rule.Validate(rule); ruleErr != nil {
				return readable(fmt.Sprintf("%s.%s.evidence[%d]", at, name, i), ruleErr)
			}
		}
		return readable(at+"."+name, nodeErr)
	}
	return ""
}

var (
	schemaChain = regexp.MustCompile(`^(?:validating [^:]+: )+`)
	chainStep   = regexp.MustCompile(`validating ([^:]+): `)
)

// readable drops the validator's chain of schema paths, keeping the message and the properties named on the way to it.
func readable(at string, err error) string {
	msg := err.Error()
	chain := schemaChain.FindString(msg)
	msg = msg[len(chain):]
	var loc []string
	if at != "" {
		loc = append(loc, at)
	}
	// Each step extends the one before it, except where a $ref jumps into $defs and starts afresh.
	prev := ""
	for _, step := range chainStep.FindAllStringSubmatch(chain, -1) {
		seg := step[1]
		added := seg
		if prev != "" && strings.HasPrefix(seg, prev) {
			added = seg[len(prev):]
		}
		parts := strings.Split(added, "/")
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "properties" {
				loc = append(loc, parts[i+1])
			}
		}
		prev = seg
	}
	if len(loc) == 0 {
		return msg
	}
	return strings.Join(loc, ".") + ": " + msg
}

type Entry struct {
	Name  string
	Path  string
	Skill *Skill
	Err   error
}

// List loads every .yaml file in dir, empty when dir does not exist.
// For the user's skills folder it adds the built-ins, a user file replacing the built-in of its name.
func List(dir string) ([]Entry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var out []Entry
	seen := map[string]bool{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, err := Load(path)
		out = append(out, Entry{Name: name, Path: path, Skill: s, Err: err})
		seen[name] = true
	}
	if inUserDir(dir) {
		built, _ := builtinFS.ReadDir("builtin")
		for _, e := range built {
			name, ok := strings.CutSuffix(e.Name(), ".yaml")
			if !ok || e.IsDir() || seen[name] {
				continue
			}
			data, _ := Builtin(e.Name())
			s, err := Parse(name, withSettings(data))
			out = append(out, Entry{Name: name, Path: "builtin/" + e.Name(), Skill: s, Err: err})
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Report is /skills: one line per skill file, valid or not, with every problem of an invalid one.
func Report(dir string) (string, error) {
	entries, err := List(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "no skills in " + dir, nil
	}
	var b strings.Builder
	for _, e := range entries {
		if e.Err == nil {
			fmt.Fprintf(&b, "- **%s** v%d: valid. Use when %s\n", e.Name, e.Skill.Version, e.Skill.When)
			continue
		}
		fmt.Fprintf(&b, "- **%s**: invalid\n", e.Name)
		var problems Problems
		if !errors.As(e.Err, &problems) {
			problems = Problems{e.Err.Error()}
		}
		for _, p := range problems {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// Validate checks files, or every skill in dir when none are given, and reports whether all are valid.
func Validate(w io.Writer, files []string, dir string) (bool, error) {
	var entries []Entry
	if len(files) == 0 {
		var err error
		if entries, err = List(dir); err != nil {
			return false, err
		}
		if len(entries) == 0 {
			fmt.Fprintln(w, "no skills in "+dir)
			return true, nil
		}
	}
	for _, f := range files {
		s, err := Load(f)
		entries = append(entries, Entry{Path: f, Skill: s, Err: err})
	}
	allValid := true
	for _, e := range entries {
		if e.Err == nil {
			fmt.Fprintf(w, "%s: ok\n", e.Path)
			continue
		}
		allValid = false
		fmt.Fprintf(w, "%s: invalid\n", e.Path)
		var problems Problems
		if !errors.As(e.Err, &problems) {
			problems = Problems{e.Err.Error()}
		}
		for _, p := range problems {
			fmt.Fprintf(w, "  - %s\n", p)
		}
	}
	return allValid, nil
}
