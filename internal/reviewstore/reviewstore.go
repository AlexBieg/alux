// Package reviewstore keeps one review per PR, pinned to the head commit its findings were checked against.
package reviewstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/diff"
	"github.com/alexbieg/alux/internal/paths"
)

type Finding struct {
	Severity  string   `json:"severity"`
	Class     string   `json:"class"`
	Path      string   `json:"path"`
	StartLine int      `json:"start_line,omitempty"`
	EndLine   int      `json:"end_line,omitempty"`
	Open      bool     `json:"open,omitempty"`
	Personas  []string `json:"personas,omitempty"`
	Problem   string   `json:"problem,omitempty"`
	Impact    string   `json:"impact,omitempty"`
	Evidence  string   `json:"evidence,omitempty"`
	Fix       string   `json:"fix,omitempty"`
	// Text is read only from reviews stored without the four fields above; review_load refuses it.
	Text string `json:"text,omitempty"`
	// OffDiff is set by Validate: the finding has lines, none of them in the diff.
	OffDiff bool `json:"off_diff,omitempty"`
	// Dismissed is set when the user dismisses the finding in the review pane.
	Dismissed bool `json:"dismissed,omitempty"`
	// Thread is the user's chat about the finding in the review pane.
	Thread []Message `json:"thread,omitempty"`
}

type Section struct{ Label, Text string }

// Sections is the finding's body in reading order, unlabelled for a review stored with Text alone.
func (f Finding) Sections() []Section {
	if f.Problem == "" && f.Text != "" {
		return []Section{{"", f.Text}}
	}
	return []Section{{"problem", f.Problem}, {"impact", f.Impact}, {"evidence", f.Evidence}, {"fix", f.Fix}}
}

// Message is one turn of a finding's thread; Role is "user" or "assistant".
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type Review struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	Head     string    `json:"head"`
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
	Loaded   time.Time `json:"loaded"`
}

var Severities = []string{"blocking", "non-blocking"}

var Verdicts = []string{"approve", "approve with comments", "request changes"}

// Validate checks the verdict and every finding against the PR's patches, keyed by path, and marks the off-diff ones;
// it returns one problem per invalid finding, and the review must not be stored while any remain.
func Validate(r *Review, patches map[string]string) []string {
	var problems []string
	if !contains(Verdicts, r.Verdict) {
		problems = append(problems, fmt.Sprintf("verdict %q is not one of %s", r.Verdict, strings.Join(Verdicts, ", ")))
	}
	return append(problems, ValidateFindings(r.Findings, patches)...)
}

// ValidateFindings is Validate without the verdict: it fills in end_line, marks the off-diff findings,
// and returns one problem per invalid finding.
func ValidateFindings(findings []Finding, patches map[string]string) []string {
	var problems []string
	for i := range findings {
		f := &findings[i]
		bad := func(format string, a ...any) {
			problems = append(problems, fmt.Sprintf("finding %d (%s): ", i, f.Path)+fmt.Sprintf(format, a...))
		}
		patch, changed := patches[f.Path]
		switch {
		case !contains(Severities, f.Severity):
			bad("severity %q is not blocking or non-blocking", f.Severity)
		case len(emptyFields(*f)) > 0:
			bad("%s empty", strings.Join(emptyFields(*f), ", "))
		case !changed:
			bad("the PR does not change this file; give the path as the diff names it")
		case f.StartLine < 0 || f.EndLine < 0:
			bad("lines must be positive")
		case f.StartLine == 0 && f.EndLine != 0:
			bad("end_line needs a start_line")
		case f.EndLine != 0 && f.EndLine < f.StartLine:
			bad("end_line %d is before start_line %d", f.EndLine, f.StartLine)
		default:
			if f.StartLine > 0 && f.EndLine == 0 {
				f.EndLine = f.StartLine
			}
			f.OffDiff = f.StartLine > 0 && !inDiff(patch, f.StartLine, f.EndLine)
		}
	}
	return problems
}

func emptyFields(f Finding) []string {
	var out []string
	for _, s := range []Section{{"problem", f.Problem}, {"impact", f.Impact}, {"evidence", f.Evidence}, {"fix", f.Fix}} {
		if strings.TrimSpace(s.Text) == "" {
			out = append(out, s.Label)
		}
	}
	return out
}

func inDiff(patch string, start, end int) bool {
	for _, n := range diff.NewLines(patch) {
		if n >= start && n <= end {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func repoDir(home, repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(owner, "..") || strings.Contains(name, "..") {
		return "", fmt.Errorf("repo %q is not owner/name", repo)
	}
	return filepath.Join(home, paths.State, "reviews", strings.ToLower(owner), strings.ToLower(name)), nil
}

func Path(home, repo string, number int) (string, error) {
	dir, err := repoDir(home, repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strconv.Itoa(number)+".json"), nil
}

// Save replaces the PR's stored review.
func Save(home string, r Review) error {
	file, err := Path(home, r.Repo, r.Number)
	if err != nil {
		return err
	}
	return writeJSON(file, r)
}

func writeJSON(file string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// Submission is one reviewer persona's validated findings for a PR at one head.
type Submission struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Head      string    `json:"head"`
	Persona   string    `json:"persona"`
	Submitted time.Time `json:"submitted"`
	Findings  []Finding `json:"findings"`
}

var personaName = regexp.MustCompile(`^[a-z0-9-]+$`)

// SubmittedPath is where persona's submission for the PR at head lives; the persona becomes the file name.
func SubmittedPath(home, repo string, number int, head, persona string) (string, error) {
	if !personaName.MatchString(persona) {
		return "", fmt.Errorf("persona %q must be lowercase letters, digits and dashes", persona)
	}
	if head == "" || strings.ContainsAny(head, `/\.`) {
		return "", fmt.Errorf("head %q is not a commit SHA", head)
	}
	dir, err := repoDir(home, repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strconv.Itoa(number)+"-submitted", head, persona+".json"), nil
}

// SaveSubmission replaces the persona's earlier submission for the same PR and head, and returns its path.
func SaveSubmission(home string, s Submission) (string, error) {
	file, err := SubmittedPath(home, s.Repo, s.Number, s.Head, s.Persona)
	if err != nil {
		return "", err
	}
	if s.Findings == nil {
		s.Findings = []Finding{}
	}
	return file, writeJSON(file, s)
}

// Dismissal is one line of the dismissed-findings log, which outlives re-reviews.
type Dismissal struct {
	Repo        string    `json:"repo"`
	Number      int       `json:"number"`
	Head        string    `json:"head"`
	DismissedAt time.Time `json:"dismissed_at"`
	Finding     Finding   `json:"finding"`
}

func DismissedLogPath(home string) string {
	return filepath.Join(home, paths.State, "reviews", "dismissed.jsonl")
}

// AppendDismissal adds d as one JSON line to the dismissed-findings log.
func AppendDismissal(home string, d Dismissal) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	file := DismissedLogPath(home)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Load is the PR's stored review, or false when it has none.
func Load(home, repo string, number int) (Review, bool, error) {
	file, err := Path(home, repo, number)
	if err != nil {
		return Review{}, false, err
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return Review{}, false, nil
	}
	if err != nil {
		return Review{}, false, err
	}
	var r Review
	if err := json.Unmarshal(data, &r); err != nil {
		return Review{}, false, fmt.Errorf("%s: %w", file, err)
	}
	return r, true, nil
}
