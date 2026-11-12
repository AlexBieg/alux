package prs

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/anthropics/anthropic-sdk-go"
)

const reviewLoadTool = "review_load"

func reviewLoadParam(target map[string]any) anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: reviewLoadTool,
		Description: anthropic.String("Store a finished local review of a pull request so the user sees each finding beside " +
			"the code in the PR file graph (/prs, g). It posts nothing to GitHub. One call is the whole review: calling " +
			"again for the PR replaces what was stored. Pass only the findings that survived triage.\n\n" +
			"Each finding is checked against the PR's current diff before anything is stored. A path the PR does not " +
			"change, a bad severity or line range, or an empty problem, impact, evidence or fix refuses the whole call with every problem listed: fix " +
			"them and call again with every finding. A line range with no line in the diff is stored and reported as " +
			"off the diff. Lines are the file's line numbers in the PR's new code; omit them for a finding about the " +
			"whole file. Set continues on a finding that is the same defect as one in the earlier stored review, so the user's " +
			"dismissal and thread move onto it; the rest are dropped, and the result names them. The review is pinned " +
			"to the PR's head commit and stops being shown once the PR moves on."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: func() map[string]any {
				props := map[string]any{
					"verdict": map[string]any{"type": "string", "enum": reviewstore.Verdicts},
					"summary": map[string]any{"type": "string", "description": "What the change does and why the verdict, in two or three sentences."},
					"findings": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"severity":   map[string]any{"type": "string", "enum": reviewstore.Severities},
								"class":      map[string]any{"type": "string", "description": "A few words naming the kind of problem."},
								"path":       map[string]any{"type": "string", "description": "File path as the diff names it, from the repo root."},
								"start_line": map[string]any{"type": "integer", "description": "First line in the new code. Omit for the whole file."},
								"end_line":   map[string]any{"type": "integer", "description": "Last line in the new code; the comment is drawn after it. Defaults to start_line."},
								"open":       map[string]any{"type": "boolean", "description": "Reviewers still disagree about it."},
								"personas":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Reviewer personas that raised it."},
								"problem":    map[string]any{"type": "string", "description": "One plain sentence: what goes wrong, as someone using or calling the code would see it. No line numbers or function names."},
								"impact":     map[string]any{"type": "string", "description": "Who hits it, when, and how bad it is if it ships unfixed."},
								"evidence":   map[string]any{"type": "string", "description": "The code path that shows it, citing path:line."},
								"fix":        map[string]any{"type": "string", "description": "The change, in a short clause, and the path:line it goes at."},
								"continues":  map[string]any{"type": "integer", "description": "Index in the PR's earlier stored review of the finding this is the same defect as, still present at the new head. Omit for a new finding."},
							},
							"required":             []string{"severity", "class", "path", "problem", "impact", "evidence", "fix"},
							"additionalProperties": false,
						},
					},
				}
				for k, v := range target {
					props[k] = v
				}
				return props
			}(),
			Required:    []string{"number", "verdict", "summary", "findings"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}
}

func (p *Poller) runReviewLoad(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		prTarget
		Verdict  string `json:"verdict"`
		Summary  string `json:"summary"`
		Findings []struct {
			reviewstore.Finding
			Continues *int `json:"continues"`
		} `json:"findings"`
	}
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	review := reviewstore.Review{Verdict: in.Verdict, Summary: in.Summary}
	continues := map[int]int{}
	for i, f := range in.Findings {
		if f.OffDiff || f.Dismissed || len(f.Thread) > 0 {
			return anthropic.NewToolResultBlock(call.ID, "off_diff, dismissed and thread are set by alux, not passed in", true)
		}
		if f.Text != "" {
			return anthropic.NewToolResultBlock(call.ID, "text is not taken; give problem, impact, evidence and fix", true)
		}
		review.Findings = append(review.Findings, f.Finding)
		if f.Continues != nil {
			continues[i] = *f.Continues
		}
	}
	label := fmt.Sprintf("PR #%d review", in.Number)
	started(call, cb, label)
	out, err := p.LoadReview(ctx, in.Repo, in.Number, review, continues)
	return finished(call, cb, label, out, err)
}

// LoadReview checks r against the PR's current diff and, if every finding is valid, stores it pinned to the head.
// continues maps a finding's index in r to the index of the same defect in the PR's stored review, whose dismissal and thread it takes over.
func (p *Poller) LoadReview(ctx context.Context, repo string, number int, r reviewstore.Review, continues map[int]int) (string, error) {
	owner, name, err := p.resolve(repo, number)
	if err != nil {
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
	patches := map[string]string{}
	for _, f := range d.Files {
		patches[f.Path] = f.Patch
	}
	r.Repo, r.Number, r.Head, r.Loaded = owner+"/"+name, number, d.HeadSHA, p.now()
	home := p.home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	prior, hasPrior, err := reviewstore.Load(home, r.Repo, number)
	if err != nil {
		if len(continues) > 0 {
			return "", err
		}
		hasPrior = false
	}
	problems := reviewstore.Validate(&r, patches)
	problems = append(problems, continuesProblems(r, prior, hasPrior, continues)...)
	if len(problems) > 0 {
		return "", fmt.Errorf("Nothing stored. Fix these and call again with every finding:\n- %s", strings.Join(problems, "\n- "))
	}
	carried := carryOver(&r, prior, hasPrior, continues)
	if err := reviewstore.Save(home, r); err != nil {
		return "", err
	}
	if p.OnReview != nil {
		p.OnReview(r)
	}
	var off []string
	for i, f := range r.Findings {
		if f.OffDiff {
			off = append(off, fmt.Sprintf("finding %d (%s:%d-%d)", i, f.Path, f.StartLine, f.EndLine))
		}
	}
	out := fmt.Sprintf("Stored the review of %s#%d at head %s: %d findings.", r.Repo, number, short(r.Head), len(r.Findings))
	if len(off) > 0 {
		out += fmt.Sprintf(" %d have no line in the diff, so they are listed under their file rather than drawn beside "+
			"a line: %s. Check those lines if that was not meant.", len(off), strings.Join(off, ", "))
	}
	return out + carried, nil
}

func continuesProblems(r, prior reviewstore.Review, hasPrior bool, continues map[int]int) []string {
	var problems []string
	taken := map[int]int{}
	for i := range r.Findings {
		c, ok := continues[i]
		switch {
		case !ok:
		case !hasPrior:
			problems = append(problems, fmt.Sprintf("finding %d: continues %d, but the PR has no earlier stored review", i, c))
		case c < 0 || c >= len(prior.Findings):
			problems = append(problems, fmt.Sprintf("finding %d: continues %d, but the earlier review has %d findings (0 to %d)", i, c, len(prior.Findings), len(prior.Findings)-1))
		default:
			if first, dup := taken[c]; dup {
				problems = append(problems, fmt.Sprintf("finding %d: continues %d, which finding %d already continues", i, c, first))
			}
			taken[c] = i
		}
	}
	return problems
}

// carryOver copies the dismissals and threads of the continued findings onto r and reports what was carried and what was dropped.
func carryOver(r *reviewstore.Review, prior reviewstore.Review, hasPrior bool, continues map[int]int) string {
	if !hasPrior {
		return ""
	}
	var carried, dropped []string
	kept := map[int]bool{}
	for i := range r.Findings {
		c, ok := continues[i]
		if !ok {
			continue
		}
		kept[c] = true
		f := &r.Findings[i]
		f.Dismissed, f.Thread = prior.Findings[c].Dismissed, slices.Clone(prior.Findings[c].Thread)
		if what := carriedWhat(*f); what != "" {
			carried = append(carried, fmt.Sprintf("finding %d (%s)", i, what))
		}
	}
	for c, f := range prior.Findings {
		if what := carriedWhat(f); what != "" && !kept[c] {
			dropped = append(dropped, fmt.Sprintf("earlier finding %d (%s, %s)", c, f.Path, what))
		}
	}
	var out string
	if len(carried) > 0 {
		out += " Carried from the earlier review: " + strings.Join(carried, ", ") + "."
	}
	if len(dropped) > 0 {
		out += " Not continued, so their dismissal or thread is gone: " + strings.Join(dropped, ", ") + "."
	}
	return out
}

// carriedWhat names the user's dismissal and thread on f, or "" when it has neither.
func carriedWhat(f reviewstore.Finding) string {
	var parts []string
	if f.Dismissed {
		parts = append(parts, "dismissed")
	}
	if n := len(f.Thread); n == 1 {
		parts = append(parts, "thread of 1 message")
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("thread of %d messages", n))
	}
	return strings.Join(parts, ", ")
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
