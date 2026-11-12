package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// stackDepth bounds how many levels above and below the listed PRs the walk reads.
const stackDepth = 4

type stackLookup struct{ field, branch string }

// Stacked is every open PR stacked above or below one of listed, by matching heads to bases, that listed does not hold.
func Stacked(ctx context.Context, gql GraphQL, slug string, listed []PR) ([]PR, error) {
	owner, name, ok := strings.Cut(slug, "/")
	if !ok {
		return nil, fmt.Errorf("prs: %q is not owner/name", slug)
	}
	known := map[int]bool{}
	for _, p := range listed {
		known[p.Number] = true
	}
	asked := map[stackLookup]bool{}
	var out []PR
	frontier := listed
	for range stackDepth {
		var lookups []stackLookup
		for _, p := range frontier {
			for _, l := range []stackLookup{{"headRefName", p.Base}, {"baseRefName", p.Head}} {
				if l.branch != "" && !asked[l] {
					asked[l] = true
					lookups = append(lookups, l)
				}
			}
		}
		frontier = nil
		for start := 0; start < len(lookups); start += mergedBatch {
			found, err := stackQuery(ctx, gql, owner, name, slug, lookups[start:min(len(lookups), start+mergedBatch)])
			if err != nil {
				return nil, err
			}
			for _, p := range found {
				if !known[p.Number] {
					known[p.Number] = true
					out = append(out, p)
					frontier = append(frontier, p)
				}
			}
		}
		if len(frontier) == 0 {
			break
		}
	}
	return out, nil
}

func stackQuery(ctx context.Context, gql GraphQL, owner, name, slug string, lookups []stackLookup) ([]PR, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "query {\n  repository(owner: %q, name: %q) {\n", owner, name)
	for i, l := range lookups {
		fmt.Fprintf(&b, "    s%d: pullRequests(%s: %q, states: OPEN, first: 10) { nodes { number title url isDraft author { login } headRefName headRefOid baseRefName } }\n", i, l.field, l.branch)
	}
	b.WriteString("  }\n}\n")
	body, err := gql(ctx, b.String())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Repository map[string]struct {
				Nodes []struct {
					Number      int    `json:"number"`
					Title       string `json:"title"`
					URL         string `json:"url"`
					IsDraft     bool   `json:"isDraft"`
					Author      *login `json:"author"`
					HeadRefName string `json:"headRefName"`
					HeadRefOid  string `json:"headRefOid"`
					BaseRefName string `json:"baseRefName"`
				} `json:"nodes"`
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
	var out []PR
	for i := range lookups {
		for _, n := range resp.Data.Repository[fmt.Sprintf("s%d", i)].Nodes {
			out = append(out, PR{Repo: slug, Number: n.Number, Title: n.Title, URL: n.URL, Draft: n.IsDraft,
				Author: n.Author.name(), Head: n.HeadRefName, HeadSHA: n.HeadRefOid, Base: n.BaseRefName})
		}
	}
	return out, nil
}
