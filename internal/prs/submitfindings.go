package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/anthropics/anthropic-sdk-go"
)

const submitFindingsTool = "submit_findings"

// SubmitTools is for reviewer subagents only: it is not in Tools or DraftTools, so the main agent never has it.
func (p *Poller) SubmitTools() []anthropic.ToolUnionParam {
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name: submitFindingsTool,
		Description: anthropic.String("Submit every finding of your review of a pull request. Each finding's anchor is checked " +
			"against the PR's diff and against the file at the PR's head before anything is stored: a path the PR does not " +
			"change, a bad severity or line range, a line past the end of the file, lines on a file the PR deletes, or an empty " +
			"problem, impact, evidence or fix refuses the whole call with every problem listed. Fix them and call again with " +
			"every finding; a call replaces your earlier submission for the PR at this head. Pass an empty list when you have " +
			"no findings. A line range with no line in the diff is stored and reported as off the diff. Lines are the file's " +
			"line numbers in the PR's new code; omit them for a finding about the whole file."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"number":  map[string]any{"type": "integer", "description": "The PR number."},
				"repo":    map[string]any{"type": "string", "description": "owner/name. Optional when only one repo is configured."},
				"persona": map[string]any{"type": "string", "description": "Your persona, as lowercase letters, digits and dashes."},
				"findings": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"severity":   map[string]any{"type": "string", "enum": reviewstore.Severities},
							"class":      map[string]any{"type": "string", "description": "A few words naming the kind of problem."},
							"path":       map[string]any{"type": "string", "description": "File path as the diff names it, from the repo root."},
							"start_line": map[string]any{"type": "integer", "description": "First line in the new code. Omit for the whole file."},
							"end_line":   map[string]any{"type": "integer", "description": "Last line in the new code. Defaults to start_line."},
							"problem":    map[string]any{"type": "string", "description": "One plain sentence: what goes wrong, as someone using or calling the code would see it. No line numbers or function names."},
							"impact":     map[string]any{"type": "string", "description": "Who hits it, when, and how bad it is if it ships unfixed."},
							"evidence":   map[string]any{"type": "string", "description": "The code path that shows it, citing path:line."},
							"fix":        map[string]any{"type": "string", "description": "The change, in a short clause, and the path:line it goes at."},
						},
						"required":             []string{"severity", "class", "path", "problem", "impact", "evidence", "fix"},
						"additionalProperties": false,
					},
				},
			},
			Required:    []string{"number", "persona", "findings"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}}
}

func (p *Poller) SubmitHandlers() map[string]agent.Handler {
	return map[string]agent.Handler{submitFindingsTool: p.runSubmitFindings}
}

func (p *Poller) runSubmitFindings(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		prTarget
		Persona  string `json:"persona"`
		Findings []struct {
			Severity  string `json:"severity"`
			Class     string `json:"class"`
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
			Problem   string `json:"problem"`
			Impact    string `json:"impact"`
			Evidence  string `json:"evidence"`
			Fix       string `json:"fix"`
		} `json:"findings"`
	}
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	findings := make([]reviewstore.Finding, len(in.Findings))
	for i, f := range in.Findings {
		findings[i] = reviewstore.Finding{Severity: f.Severity, Class: f.Class, Path: f.Path, StartLine: f.StartLine,
			EndLine: f.EndLine, Problem: f.Problem, Impact: f.Impact, Evidence: f.Evidence, Fix: f.Fix}
	}
	label := fmt.Sprintf("PR #%d findings", in.Number)
	started(call, cb, label)
	out, err := p.SubmitFindings(ctx, in.Repo, in.Number, in.Persona, findings)
	return finished(call, cb, label, out, err)
}

// SubmitFindings checks findings against the PR's diff and the files at its head and, if every one is valid,
// stores them as persona's submission for that head.
func (p *Poller) SubmitFindings(ctx context.Context, repo string, number int, persona string, findings []reviewstore.Finding) (string, error) {
	owner, name, err := p.resolve(repo, number)
	if err != nil {
		return "", err
	}
	slug := owner + "/" + name
	if _, err := reviewstore.SubmittedPath("", slug, number, "head", persona); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	d, err := FetchDiff(ctx, p.rest, owner, name, number)
	if err != nil {
		return "", err
	}
	if d.ChangedFiles > len(d.Files) {
		return "", fmt.Errorf("GitHub listed %d of the PR's %d files, so findings cannot all be checked", len(d.Files), d.ChangedFiles)
	}
	patches, removed := map[string]string{}, map[string]bool{}
	for _, f := range d.Files {
		patches[f.Path] = f.Patch
		removed[f.Path] = f.Status == "removed"
	}
	problems := reviewstore.ValidateFindings(findings, patches)

	var lined []string
	for _, f := range findings {
		if _, changed := patches[f.Path]; changed && f.StartLine > 0 && !removed[f.Path] && !slices.Contains(lined, f.Path) {
			lined = append(lined, f.Path)
		}
	}
	counts, err := fetchLineCounts(ctx, p.gql, owner, name, d.HeadSHA, lined)
	if err != nil {
		return "", err
	}
	for i, f := range findings {
		end := max(f.EndLine, f.StartLine)
		if _, changed := patches[f.Path]; !changed || f.StartLine <= 0 || (f.EndLine != 0 && f.EndLine < f.StartLine) {
			continue
		}
		bad := func(format string, a ...any) {
			problems = append(problems, fmt.Sprintf("finding %d (%s): ", i, f.Path)+fmt.Sprintf(format, a...))
		}
		if removed[f.Path] {
			bad("the PR deletes this file, so it has no lines at head; omit start_line and end_line")
		} else if n, known := counts[f.Path]; known && end > n {
			bad("line %d is past the end of the file at head, which has %d lines", end, n)
		}
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("Nothing stored. Fix these and call again with every finding:\n- %s", strings.Join(problems, "\n- "))
	}

	home := p.home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	file, err := reviewstore.SaveSubmission(home, reviewstore.Submission{
		Repo: slug, Number: number, Head: d.HeadSHA, Persona: persona, Submitted: p.now(), Findings: findings,
	})
	if err != nil {
		return "", err
	}
	var off []string
	for i, f := range findings {
		if f.OffDiff {
			off = append(off, fmt.Sprintf("finding %d (%s:%d-%d)", i, f.Path, f.StartLine, f.EndLine))
		}
	}
	out := fmt.Sprintf("Stored %d findings from %s for %s#%d at head %s: %s.", len(findings), persona, slug, number, short(d.HeadSHA), file)
	if len(off) > 0 {
		out += fmt.Sprintf(" %d have no line in the diff, so they stay listed under their file: %s. Check those lines if that was not meant.",
			len(off), strings.Join(off, ", "))
	}
	return out, nil
}

// fetchLineCounts is the number of lines of each path at sha, in one query. A path is left out when its
// blob is binary, too large to return, or missing.
func fetchLineCounts(ctx context.Context, gql GraphQL, owner, name, sha string, paths []string) (map[string]int, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "query {\n  repository(owner: %q, name: %q) {\n", owner, name)
	for i, path := range paths {
		expr, _ := json.Marshal(sha + ":" + path)
		fmt.Fprintf(&b, "    f%d: object(expression: %s) { ... on Blob { text isBinary } }\n", i, expr)
	}
	b.WriteString("  }\n}\n")
	body, err := gql(ctx, b.String())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Repository map[string]*struct {
				Text     *string `json:"text"`
				IsBinary bool    `json:"isBinary"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("prs: reading GitHub's answer: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, errors.New("prs: GitHub: " + resp.Errors[0].Message)
	}
	counts := map[string]int{}
	for i, path := range paths {
		blob := resp.Data.Repository[fmt.Sprintf("f%d", i)]
		if blob == nil || blob.IsBinary || blob.Text == nil {
			continue
		}
		text := *blob.Text
		n := strings.Count(text, "\n")
		if text != "" && !strings.HasSuffix(text, "\n") {
			n++
		}
		counts[path] = n
	}
	return counts, nil
}
