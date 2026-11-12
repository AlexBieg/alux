// Package guardian asks a model what a shell command does, judges it against an agent's criteria, and logs the outcome.
package guardian

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

const (
	defaultURL     = "http://127.0.0.1:11434"
	defaultModel   = "d3:2b"
	requestTimeout = 10 * time.Second
	checkTimeout   = 2 * time.Second
	keepAlive      = "30m"
	minOllama      = "0.40.9"
)

// Thresholds fitted for d3:2b at 95% recall of stops on escalated commands; refit when the model changes.
const (
	MainThreshold     = 0.26
	BuildThreshold    = 0.13
	ReadOnlyThreshold = 0.05
)

// Category is one kind of thing a command can do, judged by its own probability, never as a scale.
type Category string

const (
	Read    Category = "read"
	Build   Category = "build"
	Edit    Category = "edit"
	Record  Category = "record"
	Rewrite Category = "rewrite"
	Destroy Category = "destroy"
	Network Category = "network"
	System  Category = "system"
)

// Categories is the rubric's order, which is also how the model's answer is indexed.
var Categories = []Category{Read, Build, Edit, Record, Rewrite, Destroy, Network, System}

var descriptions = map[Category]string{
	Read:    "Only reads, searches, lists or prints, for example: ls, cat, rg, git log, git diff, jj status, jj log",
	Build:   "Builds, tests, lints or type-checks, for example: go test, go vet, make build, cargo build, pytest, tsc --noEmit",
	Edit:    "Creates, modifies, moves or deletes files inside the project, for example: gofmt -w, sed -i, mkdir, touch, mv, rm of a project file",
	Record:  "Records or reshapes work in version control in a way its history can undo, for example: jj commit, jj describe, jj new, jj squash, jj abandon, jj rebase, git add, git commit, git checkout -b",
	Rewrite: "Rewrites or discards work in version control so it cannot be recovered, for example: git reset --hard, git clean -fdx, git rebase, git branch -D, jj op abandon, jj util gc",
	Destroy: "Permanently deletes, wipes or overwrites data that is not easily recovered, for example: rm -rf outside the project, dd, shred, rsync --delete, aws s3 rm, gh repo delete, emptying a database",
	Network: "Uses the network or publishes, for example: git push, git fetch, jj git push, curl, wget, npm install, pip install, npm publish, gh pr merge, gh release create, ssh",
	System:  "Changes things outside the project, for example: editing ~/.zshrc, brew install, sudo, kill, crontab, launchctl, defaults write",
}

// Read through questions(), never directly: the order of the criteria is the index of the answer.
func questions() map[string]any {
	criteria := make([]string, len(Categories))
	for i, c := range Categories {
		criteria[i] = descriptions[c]
	}
	return map[string]any{"does": map[string]any{
		"type":         "score",
		"instructions": "What does this shell command do? If it does several things, choose the riskiest thing it does.",
		"criteria":     criteria,
	}}
}

// Criteria is one agent's work: a command whose probability outside Allowed reaches Threshold is stopped.
type Criteria struct {
	Agent     string
	Allowed   []Category
	Threshold float64
}

func (c Criteria) Validate() error {
	if strings.TrimSpace(c.Agent) == "" {
		return fmt.Errorf("command criteria name no agent")
	}
	if len(c.Allowed) == 0 {
		return fmt.Errorf("agent %s's command criteria allow nothing; a read-only agent allows %q", c.Agent, Read)
	}
	for _, a := range c.Allowed {
		if !slices.Contains(Categories, a) {
			return fmt.Errorf("agent %s's command criteria allow %q, which is not a category", c.Agent, a)
		}
	}
	if c.Threshold <= 0 || c.Threshold >= 1 {
		return fmt.Errorf("agent %s's command threshold %.2f is not between 0 and 1", c.Agent, c.Threshold)
	}
	return nil
}

// Verdict is what the model made of one command for one agent. The zero value is no verdict.
type Verdict struct {
	Agent string
	// Probabilities is the model's answer per category; Danger is the share outside the agent's Allowed.
	Probabilities map[Category]float64
	Danger        float64
	Threshold     float64
	Flagged       bool
	// Top is the single most likely category, for a glance.
	Top Category
	// Answers is the model's raw answers object, kept as a string so a Verdict stays comparable.
	Answers string
}

func (v Verdict) Known() bool { return v.Top != "" }

type Conf struct {
	URL   string
	Model string
	// Log is the JSONL file every shell command's verdict and decision is appended to.
	Log string
}

// Config reads settings.json's {"guardian": {"url", "model", "log"}}; the block and every field are optional.
func Config() Conf {
	home, _ := os.UserHomeDir()
	var settings struct {
		Guardian struct {
			URL   string `json:"url"`
			Model string `json:"model"`
			Log   string `json:"log"`
		} `json:"guardian"`
	}
	if home != "" {
		if data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json")); err == nil {
			_ = json.Unmarshal(data, &settings)
		}
	}
	c := Conf{
		URL:   strings.TrimRight(strings.TrimSpace(settings.Guardian.URL), "/"),
		Model: strings.TrimSpace(settings.Guardian.Model),
		Log:   strings.TrimSpace(os.ExpandEnv(settings.Guardian.Log)),
	}
	if c.Log == "" && home != "" {
		c.Log = filepath.Join(home, paths.Share, "guardian/verdicts.jsonl")
	}
	if c.URL == "" {
		c.URL = defaultURL
	}
	if c.Model == "" {
		c.Model = defaultModel
	}
	return c
}

// Check asks the server once, briefly, whether the model is pulled; the error says what to do about it.
func Check(ctx context.Context, conf Conf) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, conf.URL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("guardian: Ollama is not answering at %s; install it from https://ollama.com, start it, then run `ollama pull %s`", conf.URL, conf.Model)
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&tags)
	}
	if v := ollamaVersion(ctx, conf.URL); olderThan(v, minOllama) {
		return fmt.Errorf("guardian: Ollama %s is older than %s, which the guardian model needs; run: brew upgrade ollama", v, minOllama)
	}
	want := conf.Model
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, m := range tags.Models {
		if m.Name == want {
			return nil
		}
	}
	return fmt.Errorf("guardian: model %s is not pulled; run `ollama pull %s`", conf.Model, conf.Model)
}

// ollamaVersion is "" when the server does not say.
func ollamaVersion(ctx context.Context, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/version", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var body struct {
		Version string `json:"version"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Version)
}

// olderThan ignores any -suffix; an unparsable version is never older.
func olderThan(v, min string) bool {
	a, b := versionParts(v), versionParts(min)
	if a == nil || b == nil {
		return false
	}
	for i := range max(len(a), len(b)) {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// Client is a handle on an Ollama server someone else runs.
type Client struct {
	conf    Conf
	http    *http.Client
	onError func(string)

	mu   sync.Mutex
	down bool

	logMu     sync.Mutex
	logFailed bool
}

// New does not contact the server: Ollama may be started after alux, and every request
// tries again.
func New(conf Conf, onError func(string)) *Client {
	return &Client{conf: conf, http: &http.Client{Timeout: requestTimeout}, onError: onError}
}

// ask posts one set of questions about command; false means the server failed and has been reported.
func (c *Client) ask(ctx context.Context, command string, qs map[string]any) (json.RawMessage, time.Duration, bool) {
	return c.askAbout(ctx, map[string]string{"command": command}, qs)
}

func (c *Client) askAbout(ctx context.Context, state map[string]string, qs map[string]any) (json.RawMessage, time.Duration, bool) {
	body, err := json.Marshal(map[string]any{
		"model":      c.conf.Model,
		"keep_alive": keepAlive,
		"state":      state,
		"questions":  qs,
	})
	if err != nil {
		return nil, 0, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.conf.URL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.failed(fmt.Sprintf("guardian: %s is not answering, so every shell command now needs your approval or is refused; start it with `ollama serve` (%v)", c.conf.URL, err))
		return nil, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.failed(fmt.Sprintf("guardian: %s returned %s for model %s", c.conf.URL, resp.Status, c.conf.Model))
		return nil, 0, false
	}
	var raw struct {
		Answers json.RawMessage `json:"answers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	return raw.Answers, time.Since(start), true
}

// Classify reports the zero Verdict for every failure, including a nil Client.
func (c *Client) Classify(ctx context.Context, command string, crit Criteria) Verdict {
	if c == nil || c.http == nil {
		return Verdict{}
	}
	raw, _, ok := c.ask(ctx, command, questions())
	if !ok {
		return Verdict{}
	}
	var answers struct {
		Does struct {
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"does"`
	}
	if json.Unmarshal(raw, &answers) != nil || len(answers.Does.Probabilities) != len(Categories) {
		c.failed("guardian: the answer did not carry a probability for every category")
		return Verdict{}
	}
	probs := make(map[Category]float64, len(Categories))
	for i, cat := range Categories {
		probs[cat] = answers.Does.Probabilities[strconv.Itoa(i)]
	}
	c.recovered()
	v := judge(probs, crit)
	v.Answers = string(raw)
	return v
}

// SuggestThreshold is the share of a command's work guardian must give the file tools before one is suggested.
const SuggestThreshold = 0.42

const shell = "bash"

type option struct{ Key, Desc string }

// menu is choice criteria that keep their order on the wire, because the order sways the model.
type menu []option

func (m menu) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, o := range m {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(o.Key)
		if err != nil {
			return nil, err
		}
		desc, err := json.Marshal(o.Desc)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(desc)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (m menu) keys() []string {
	keys := make([]string, len(m))
	for i, o := range m {
		keys[i] = o.Key
	}
	return keys
}

var suggestOptions = menu{
	{"view", "Shows the text of files or lists a directory, for example: cat a.go, sed -n 10,40p a.go, head -50 a.go, ls internal"},
	{"grep", "Looks for a pattern inside files and shows where it appears, for example: grep -rn Foo internal, rg -l TODO"},
	{"glob", "Looks for files by their name, for example: find . -name '*.go', ls internal/*/*_test.go, fd -e md"},
	{shell, "Runs a program, builds, tests, uses version control, or counts, sorts or reshapes output, for example: go test, jj log, git show, wc -l, grep -c, sort | uniq, jq"},
}

// suggestMenu keeps the file tools the agent has, in their fixed order, and always ends with bash.
func suggestMenu(tools []string) menu {
	var m menu
	for _, o := range suggestOptions {
		if o.Key == shell || slices.Contains(tools, o.Key) {
			m = append(m, o)
		}
	}
	return m
}

func suggestQuestions(m menu) map[string]any {
	return map[string]any{"suggest": map[string]any{
		"type":         "choice",
		"instructions": "What is this shell command doing? If it does several things, choose the one that needs the most.",
		"criteria":     m,
	}}
}

// Suggestion is which file tool guardian thinks could do a command's work. The zero value is none.
type Suggestion struct {
	// Menu is the options asked about, bash last.
	Menu          []string
	Probabilities map[string]float64
	// Score is 1 − P(bash).
	Score float64
	// Tools is empty when nothing cleared SuggestThreshold.
	Tools       []string
	Took        time.Duration
	Answers     string
	QuestionsID string
	// Source is SourceRule when alux named the tools by pattern, and empty when guardian did.
	Source string
}

const SourceRule = "rule"

func (s Suggestion) Known() bool { return len(s.Probabilities) > 0 }

// Suggest asks which of tools (view, grep, glob) could do command's work, and reports the zero value for every failure.
func (c *Client) Suggest(ctx context.Context, command string, tools []string) Suggestion {
	m := suggestMenu(tools)
	if c == nil || c.http == nil || len(m) < 2 {
		return Suggestion{}
	}
	raw, took, ok := c.ask(ctx, command, suggestQuestions(m))
	if !ok {
		return Suggestion{}
	}
	var answers struct {
		Suggest struct {
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"suggest"`
	}
	keys := m.keys()
	if json.Unmarshal(raw, &answers) != nil {
		c.failed("guardian: the answer to the tool question could not be read")
		return Suggestion{}
	}
	probs := make(map[string]float64, len(keys))
	for _, k := range keys {
		p, found := answers.Suggest.Probabilities[k]
		if !found {
			c.failed("guardian: the answer did not carry a probability for every tool")
			return Suggestion{}
		}
		probs[k] = p
	}
	c.recovered()
	score, picked := pick(probs, keys)
	return Suggestion{
		Menu: keys, Probabilities: probs, Score: score, Tools: picked, Took: took,
		Answers: string(raw), QuestionsID: SuggestionID(keys),
	}
}

// pick suggests the likeliest file tool once the score clears the threshold, and a second only if it clears it alone.
func pick(probs map[string]float64, keys []string) (float64, []string) {
	score := 1 - probs[shell]
	if score < SuggestThreshold {
		return score, nil
	}
	var ranked []string
	for _, k := range keys {
		if k != shell {
			ranked = append(ranked, k)
		}
	}
	if len(ranked) == 0 {
		return score, nil
	}
	slices.SortStableFunc(ranked, func(a, b string) int { return cmp.Compare(probs[b], probs[a]) })
	if len(ranked) > 1 && probs[ranked[1]] >= SuggestThreshold {
		return score, ranked[:2]
	}
	return score, ranked[:1]
}

// Task tiers, smallest first.
const (
	TierSmall  = "small"
	TierMedium = "medium"
	TierLarge  = "large"
)

var tierOptions = menu{
	{TierSmall, "Small: a short, simple task. Create or change one small file, run a few given commands, or find one thing in the code and report it."},
	{TierMedium, "Ordinary engineering: a clear goal in known code that needs some reading, a few files and running the tests, but no open design question."},
	{TierLarge, "Hard: an ambiguous or open goal, design choices to make, changes across many files or systems, subtle correctness, or research that must weigh many sources."},
}

// tierChars caps the task text sent, as in the replay the tier question was fitted on.
const tierChars = 6000

func tierQuestions(m menu) map[string]any {
	return map[string]any{"tier": map[string]any{
		"type":         "choice",
		"instructions": "Which model tier should a coding agent use for this task? Judge how hard the task is.",
		"criteria":     m,
	}}
}

// Tier is how likely task is to be small, medium or large work, asked in both option orders and averaged
// because the model leans to the first option; false for every failure.
func (c *Client) Tier(ctx context.Context, task string) (map[string]float64, bool) {
	if c == nil || c.http == nil {
		return nil, false
	}
	if len(task) > tierChars {
		task = strings.ToValidUTF8(task[:tierChars], "")
	}
	reversed := slices.Clone(tierOptions)
	slices.Reverse(reversed)
	sum := map[string]float64{}
	for _, m := range []menu{tierOptions, reversed} {
		raw, _, ok := c.askAbout(ctx, map[string]string{"task": task}, tierQuestions(m))
		if !ok {
			return nil, false
		}
		var answers struct {
			Tier struct {
				Probabilities map[string]float64 `json:"probabilities"`
			} `json:"tier"`
		}
		if json.Unmarshal(raw, &answers) != nil {
			c.failed("guardian: the answer to the tier question could not be read")
			return nil, false
		}
		for _, k := range m.keys() {
			p, found := answers.Tier.Probabilities[k]
			if !found {
				c.failed("guardian: the answer did not carry a probability for every tier")
				return nil, false
			}
			sum[k] += p / 2
		}
	}
	c.recovered()
	return sum, true
}

// SuggestionID names the tool question asked of an agent with these tools by content, like QuestionsID.
func SuggestionID(tools []string) string {
	data, _ := json.Marshal(suggestQuestions(suggestMenu(tools)))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

func judge(probs map[Category]float64, crit Criteria) Verdict {
	v := Verdict{Agent: crit.Agent, Probabilities: probs, Threshold: crit.Threshold, Top: Categories[0]}
	for _, cat := range Categories {
		if probs[cat] > probs[v.Top] {
			v.Top = cat
		}
		if !slices.Contains(crit.Allowed, cat) {
			v.Danger += probs[cat]
		}
	}
	v.Flagged = v.Danger >= crit.Threshold
	return v
}

var does = map[Category]string{
	Read:    "reads files",
	Build:   "builds or tests",
	Edit:    "edits project files",
	Record:  "records in version control",
	Rewrite: "rewrites history",
	Destroy: "deletes data",
	Network: "uses the network",
	System:  "changes the system",
}

// Does is what the command does by its most likely category, as a person would put it.
func (v Verdict) Does() string {
	if d := does[v.Top]; d != "" {
		return d
	}
	return string(v.Top)
}

// Risk is low for a command guardian passed, medium for a stop, and high for a stop at 0.75 or more.
func (v Verdict) Risk() string {
	switch {
	case !v.Flagged:
		return "low"
	case v.Danger < 0.75:
		return "medium"
	}
	return "high"
}

// Says is the verdict in words, for a person or an agent.
func (v Verdict) Says() string {
	return fmt.Sprintf("guardian thinks it %s · %s risk", v.Does(), v.Risk())
}

// Gate binds an agent's criteria to the client; with no client it judges nothing, which stops every command.
type Gate struct {
	Client   *Client
	Criteria Criteria
	// Allowed, when set, replaces Criteria.Allowed on every command, so a mode switch applies to the next one.
	Allowed func() []Category
	// Unattended reports a run nobody can be asked about, so a refusal there is not logged as a person's.
	Unattended func() bool
}

func (g Gate) Screen(ctx context.Context, command string) Verdict {
	crit := g.Criteria
	if g.Allowed != nil {
		crit.Allowed = g.Allowed()
	}
	return g.Client.Classify(ctx, command, crit)
}

func (g Gate) Record(command string, v Verdict, decision string) {
	g.RecordSuggested(command, v, Suggestion{}, decision, "")
}

func (g Gate) Suggest(ctx context.Context, command string, tools []string) Suggestion {
	return g.Client.Suggest(ctx, command, tools)
}

// RecordSuggested logs a command's verdict, the tool guardian suggested for it, and what became of the suggestion, as one line.
func (g Gate) RecordSuggested(command string, v Verdict, s Suggestion, decision, outcome string) {
	if decision == "declined" && g.Unattended != nil && g.Unattended() {
		decision = "refused"
		if outcome == "declined→declined" {
			outcome = "declined→refused"
		}
	}
	g.Client.Log(Outcome{Command: command, Agent: g.Criteria.Agent, Decision: decision, Verdict: v,
		Suggestion: s, SuggestionOutcome: outcome})
}

// Outcome is one shell command as it went: what guardian said, and what happened to it.
type Outcome struct {
	Command string
	Agent   string
	// Decision is passed, suggested, approved, declined, refused (nobody to ask) or interrupted.
	Decision   string
	Verdict    Verdict
	Suggestion Suggestion
	// SuggestionOutcome is accepted, ignored, or declined→ the decision; empty when nothing was suggested.
	SuggestionOutcome string
}

type entry struct {
	Time          time.Time            `json:"time"`
	Command       string               `json:"command"`
	Dir           string               `json:"dir"`
	Agent         string               `json:"agent"`
	Decision      string               `json:"decision"`
	Model         string               `json:"model"`
	Questions     string               `json:"questions"`
	Threshold     float64              `json:"threshold,omitempty"`
	Danger        *float64             `json:"danger,omitempty"`
	Top           Category             `json:"top,omitempty"`
	Probabilities map[Category]float64 `json:"probabilities,omitempty"`
	Flagged       bool                 `json:"flagged"`
	// Human is "safe" or "unsafe" when a person decided, and absent otherwise.
	Human string `json:"human,omitempty"`
	// Misclassified is set only when a flagged verdict was put to a person.
	Misclassified *bool           `json:"misclassified,omitempty"`
	Answers       json.RawMessage `json:"answers,omitempty"`

	SuggestionQuestions     string             `json:"suggestion_questions,omitempty"`
	SuggestionScore         *float64           `json:"suggestion_score,omitempty"`
	SuggestionProbabilities map[string]float64 `json:"suggestion_probabilities,omitempty"`
	SuggestedTools          []string           `json:"suggested_tools,omitempty"`
	SuggestionOutcome       string             `json:"suggestion_outcome,omitempty"`
	SuggestionSource        string             `json:"suggestion_source,omitempty"`
	SuggestionMs            *int64             `json:"suggestion_ms,omitempty"`
	SuggestionAnswers       json.RawMessage    `json:"suggestion_answers,omitempty"`
}

// Log appends a record, saving the questions that produced it beside the log so a new prompt can be replayed against it.
func (c *Client) Log(r Outcome) {
	if c == nil || c.conf.Log == "" {
		return
	}
	id := QuestionsID()
	dir, _ := os.Getwd()
	e := entry{
		Time: time.Now().UTC(), Command: r.Command, Dir: dir, Agent: r.Agent, Decision: r.Decision,
		Model: c.conf.Model, Questions: id,
	}
	if v := r.Verdict; v.Known() {
		danger := v.Danger
		e.Danger, e.Threshold, e.Top, e.Probabilities, e.Flagged = &danger, v.Threshold, v.Top, v.Probabilities, v.Flagged
		e.Answers = json.RawMessage(v.Answers)
	}
	switch r.Decision {
	case "approved":
		e.Human = "safe"
	case "declined":
		e.Human = "unsafe"
	}
	if e.Human != "" && r.Verdict.Flagged {
		wrong := e.Human == "safe"
		e.Misclassified = &wrong
	}
	saves := []saved{{id, questions()}}
	if s := r.Suggestion; s.Known() {
		score, ms := s.Score, s.Took.Milliseconds()
		e.SuggestionQuestions, e.SuggestionScore, e.SuggestionProbabilities = s.QuestionsID, &score, s.Probabilities
		e.SuggestedTools, e.SuggestionMs, e.SuggestionAnswers = s.Tools, &ms, json.RawMessage(s.Answers)
		saves = append(saves, saved{s.QuestionsID, suggestQuestions(suggestMenu(s.Menu))})
	}
	if s := r.Suggestion; s.Source != "" {
		e.SuggestedTools, e.SuggestionSource = s.Tools, s.Source
	}
	e.SuggestionOutcome = r.SuggestionOutcome
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	c.logMu.Lock()
	defer c.logMu.Unlock()
	if err := c.append(line, saves); err != nil && !c.logFailed {
		c.logFailed = true
		if c.onError != nil {
			c.onError("guardian: could not write the verdict log: " + err.Error())
		}
	}
}

type saved struct {
	id        string
	questions map[string]any
}

func (c *Client) append(line []byte, saves []saved) error {
	dir := filepath.Dir(c.conf.Log)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, s := range saves {
		qpath := filepath.Join(dir, "questions-"+s.id+".json")
		if _, err := os.Stat(qpath); os.IsNotExist(err) {
			data, _ := json.MarshalIndent(s.questions, "", "  ")
			if err := os.WriteFile(qpath, append(data, '\n'), 0o600); err != nil {
				return err
			}
		}
	}
	f, err := os.OpenFile(c.conf.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// QuestionsID names the current questions by content, so log lines from different prompts never mix.
func QuestionsID() string {
	data, _ := json.Marshal(questions())
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

// failed reports the first failure of a run of them, so a stopped server is one notice rather than
// one per command, and a server that comes back and goes down again is reported again.
func (c *Client) failed(msg string) {
	c.mu.Lock()
	first := !c.down
	c.down = true
	c.mu.Unlock()
	if first && c.onError != nil {
		c.onError(msg)
	}
}

func (c *Client) recovered() {
	c.mu.Lock()
	c.down = false
	c.mu.Unlock()
}
