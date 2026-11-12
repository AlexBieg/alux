package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/anthropics/anthropic-sdk-go"
)

const findingsTool = "pr_findings"

var areasPersona = regexp.MustCompile(`^- ([a-z][a-z0-9-]*) \|`)

func findingsParam() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: findingsTool,
		Description: anthropic.String("Every finding the reviewer personas named in an areas file submitted with submit_findings " +
			"for a pull request at its current head, one per line. Fails naming each persona with no submission at that head.\n\n" +
			"Read-only, and no approval is asked for it."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"number": map[string]any{"type": "integer", "description": "The PR number."},
				"repo":   map[string]any{"type": "string", "description": "owner/name. Optional when only one repo is configured."},
				"areas":  map[string]any{"type": "string", "description": `Path of a file with one "- <persona id> | ..." line per reviewer.`},
			},
			Required:    []string{"number", "areas"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}
}

func (p *Poller) runFindings(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		prTarget
		Areas string `json:"areas"`
	}
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	label := fmt.Sprintf("PR #%d findings", in.Number)
	started(call, cb, label)
	out, err := p.Findings(ctx, in.Repo, in.Number, in.Areas)
	return finished(call, cb, label, out, err)
}

// Findings renders the submissions of the personas listed in the areas file for the PR at its head.
func (p *Poller) Findings(ctx context.Context, repo string, number int, areas string) (string, error) {
	owner, name, err := p.resolve(repo, number)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(areas)
	if err != nil {
		return "", err
	}
	var personas []string
	for line := range strings.Lines(string(data)) {
		if m := areasPersona.FindStringSubmatch(line); m != nil && !slices.Contains(personas, m[1]) {
			personas = append(personas, m[1])
		}
	}
	if len(personas) == 0 {
		return "", fmt.Errorf(`%s names no persona on a "- <persona id> |" line`, areas)
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	d, err := fetchPR(ctx, p.rest, owner, name, number)
	if err != nil {
		return "", err
	}
	home := p.home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	slug := owner + "/" + name
	var lines, missing []string
	for _, persona := range personas {
		file, err := reviewstore.SubmittedPath(home, slug, number, d.HeadSHA, persona)
		if err != nil {
			return "", err
		}
		raw, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, persona)
			continue
		}
		if err != nil {
			return "", err
		}
		var s reviewstore.Submission
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("%s: %w", file, err)
		}
		for _, f := range s.Findings {
			lines = append(lines, findingLine(persona, f))
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("no submission for %s#%d at head %s from: %s", slug, number, short(d.HeadSHA), strings.Join(missing, ", "))
	}
	if len(lines) == 0 {
		lines = []string{"- none | no findings"}
	}
	return fmt.Sprintf("Findings submitted for %s#%d at head %s by %s:\n%s", slug, number, short(d.HeadSHA),
		strings.Join(personas, ", "), strings.Join(lines, "\n")), nil
}

func findingLine(persona string, f reviewstore.Finding) string {
	at := f.Path
	if f.StartLine > 0 {
		at += fmt.Sprintf(":%d", f.StartLine)
		if f.EndLine > f.StartLine {
			at += fmt.Sprintf("-%d", f.EndLine)
		}
	}
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	return fmt.Sprintf("- %s | %s | %s | %s | problem: %s | impact: %s | evidence: %s | fix: %s",
		persona, f.Severity, flat(f.Class), at, flat(f.Problem), flat(f.Impact), flat(f.Evidence), flat(f.Fix))
}
