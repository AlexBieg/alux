package prs

import (
	"context"
	"strings"
	"testing"
)

func TestMergedReadsEachHeadsMergedPRsAndSkipsTheRest(t *testing.T) {
	var asked string
	got, err := Merged(context.Background(), func(_ context.Context, q string) ([]byte, error) {
		asked = q
		return []byte(`{"data": {"repository": {
		  "h0": {"nodes": [{"number": 12, "headRefOid": "5d6c", "baseRefName": "main", "mergeCommit": {"oid": "fd32"}}]},
		  "h1": {"nodes": []}}}}`), nil
	}, "acme/app", []string{"abc-768", "abc-999"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked, `repository(owner: "acme", name: "app")`) || !strings.Contains(asked, `headRefName: "abc-999", states: MERGED`) {
		t.Errorf("the query did not ask for both heads' merged PRs:\n%s", asked)
	}
	want := MergedPR{Number: 12, HeadSHA: "5d6c", Base: "main", MergeSHA: "fd32"}
	if len(got["abc-768"]) != 1 || got["abc-768"][0] != want || len(got["abc-999"]) != 0 {
		t.Errorf("merged is %+v", got)
	}
}
