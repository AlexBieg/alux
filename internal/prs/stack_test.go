package prs

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"testing"
)

// openPRs answers each pullRequests(headRefName|baseRefName: …) alias from a fixed set of open PRs.
func openPRs(open []PR) GraphQL {
	lookup := regexp.MustCompile(`(s\d+): pullRequests\((\w+): "([^"]*)"`)
	return func(_ context.Context, query string) ([]byte, error) {
		repo := map[string]any{}
		for _, m := range lookup.FindAllStringSubmatch(query, -1) {
			nodes := []map[string]any{}
			for _, p := range open {
				if (m[2] == "headRefName" && p.Head == m[3]) || (m[2] == "baseRefName" && p.Base == m[3]) {
					nodes = append(nodes, map[string]any{"number": p.Number, "title": p.Title, "headRefName": p.Head, "baseRefName": p.Base})
				}
			}
			repo[m[1]] = map[string]any{"nodes": nodes}
		}
		return json.Marshal(map[string]any{"data": map[string]any{"repository": repo}})
	}
}

func TestStackedFindsEveryUnlistedLevelAboveAndBelow(t *testing.T) {
	open := []PR{
		{Number: 1, Head: "api", Base: "main"},
		{Number: 2, Head: "ui", Base: "api"},
		{Number: 3, Head: "copy", Base: "ui"},
		{Number: 4, Head: "polish", Base: "copy"},
		{Number: 9, Head: "other", Base: "main"},
	}
	got, err := Stacked(context.Background(), openPRs(open), "acme/web", []PR{open[1]})
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, p := range got {
		numbers = append(numbers, p.Number)
	}
	slices.Sort(numbers)
	if !slices.Equal(numbers, []int{1, 3, 4}) {
		t.Fatalf("found %v, want the base #1 and both PRs above, #3 and #4, but not the listed #2 or the unrelated #9", numbers)
	}
}
