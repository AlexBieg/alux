package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	maxPages   = 10
	pageFields = "pageInfo { hasNextPage endCursor }"
)

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type connection[T any] struct {
	PageInfo pageInfo `json:"pageInfo"`
	Nodes    []T      `json:"nodes"`
}

// all is c's nodes and those of its later pages: field(after) selects the next page under repository, found at path.
func (c connection[T]) all(ctx context.Context, gql GraphQL, owner, name, path string, field func(after string) string) ([]T, error) {
	nodes, page := c.Nodes, c.PageInfo
	for i := 1; i < maxPages && page.HasNextPage && page.EndCursor != ""; i++ {
		body, err := gql(ctx, fmt.Sprintf("query {\n  repository(owner: %q, name: %q) {\n    %s\n  }\n}", owner, name, field(page.EndCursor)))
		if err != nil {
			return nil, err
		}
		var resp struct {
			Data struct {
				Repository json.RawMessage `json:"repository"`
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
		raw := resp.Data.Repository
		for _, key := range strings.Split(path, ".") {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil || m == nil {
				return nil, fmt.Errorf("prs: GitHub's answer has no %s in %s/%s", path, owner, name)
			}
			raw = m[key]
		}
		var next connection[T]
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, fmt.Errorf("prs: reading %s: %w", path, err)
		}
		nodes, page = append(nodes, next.Nodes...), next.PageInfo
	}
	return nodes, nil
}

const checkFields = `__typename
      ... on CheckRun { name status conclusion }
      ... on StatusContext { context state }`

type checkContext struct {
	Type       string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Context    string `json:"context"`
	State      string `json:"state"`
}

const headCommitFields = `commits(last: 1) { nodes { commit { oid committedDate statusCheckRollup { state contexts(first: 100) { ` + pageFields + ` nodes {
      ` + checkFields + `
    } } } } } }`

// headCommit is the PR's last commit and the first page of its checks.
type headCommit struct {
	Nodes []struct {
		Commit struct {
			Oid           string    `json:"oid"`
			CommittedDate time.Time `json:"committedDate"`
			Rollup        *struct {
				State    string                   `json:"state"`
				Contexts connection[checkContext] `json:"contexts"`
			} `json:"statusCheckRollup"`
		} `json:"commit"`
	} `json:"nodes"`
}

// checks is every check on the head commit; later pages are read by oid so a push between pages cannot mix commits.
func (h headCommit) checks(ctx context.Context, gql GraphQL, owner, name string) (state string, all []checkContext, err error) {
	n := len(h.Nodes)
	if n == 0 || h.Nodes[n-1].Commit.Rollup == nil {
		return "", nil, nil
	}
	c := h.Nodes[n-1].Commit
	all, err = c.Rollup.Contexts.all(ctx, gql, owner, name, "object.statusCheckRollup.contexts", func(after string) string {
		return fmt.Sprintf("object(oid: %q) { ... on Commit { statusCheckRollup { contexts(first: 100, after: %q) { %s nodes {\n      %s\n    } } } } }",
			c.Oid, after, pageFields, checkFields)
	})
	return c.Rollup.State, all, err
}
