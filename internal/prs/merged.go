package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MergedPR is a merged pull request: the head commit GitHub merged, the branch it merged into, and the commit the merge made there.
type MergedPR struct {
	Number                  int
	HeadSHA, Base, MergeSHA string
}

// mergedBatch keeps one query's aliases well under GitHub's node limits.
const mergedBatch = 40

// Merged is every merged PR in the repo slug whose head branch is one of heads, by head branch.
func Merged(ctx context.Context, gql GraphQL, slug string, heads []string) (map[string][]MergedPR, error) {
	owner, name, ok := strings.Cut(slug, "/")
	if !ok {
		return nil, fmt.Errorf("prs: %q is not owner/name", slug)
	}
	out := map[string][]MergedPR{}
	for start := 0; start < len(heads); start += mergedBatch {
		batch := heads[start:min(len(heads), start+mergedBatch)]
		var b strings.Builder
		fmt.Fprintf(&b, "query {\n  repository(owner: %q, name: %q) {\n", owner, name)
		for i, h := range batch {
			fmt.Fprintf(&b, "    h%d: pullRequests(headRefName: %q, states: MERGED, first: 10) { nodes { number headRefOid baseRefName mergeCommit { oid } } }\n", i, h)
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
						HeadRefOid  string `json:"headRefOid"`
						BaseRefName string `json:"baseRefName"`
						MergeCommit *struct {
							Oid string `json:"oid"`
						} `json:"mergeCommit"`
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
		for i, h := range batch {
			for _, n := range resp.Data.Repository[fmt.Sprintf("h%d", i)].Nodes {
				pr := MergedPR{Number: n.Number, HeadSHA: n.HeadRefOid, Base: n.BaseRefName}
				if n.MergeCommit != nil {
					pr.MergeSHA = n.MergeCommit.Oid
				}
				out[h] = append(out[h], pr)
			}
		}
	}
	return out, nil
}
