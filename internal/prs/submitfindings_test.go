package prs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/reviewstore"
)

const head = "0123456789abcdef"

var blobAlias = regexp.MustCompile(`(f\d+): object\(expression: ("[^"]*")\)`)

// fakeBlobs answers the one blob query from texts keyed by path, counting the queries it saw.
func fakeBlobs(t *testing.T, texts map[string]string, queries *int) GraphQL {
	return func(_ context.Context, query string) ([]byte, error) {
		*queries++
		repo := map[string]any{}
		for _, m := range blobAlias.FindAllStringSubmatch(query, -1) {
			var expr string
			if err := json.Unmarshal([]byte(m[2]), &expr); err != nil {
				t.Fatal(err)
			}
			path, ok := strings.CutPrefix(expr, head+":")
			if !ok {
				t.Fatalf("expression %q is not pinned to the head", expr)
			}
			if text, found := texts[path]; found {
				repo[m[1]] = map[string]any{"text": text, "isBinary": false}
			} else {
				repo[m[1]] = nil
			}
		}
		return json.Marshal(map[string]any{"data": map[string]any{"repository": repo}})
	}
}

func lines(n int) string { return strings.Repeat("x\n", n) }

func submitPoller(t *testing.T, queries *int) *Poller {
	rest, _ := fakeREST(t, []map[string]any{
		{"filename": "src/a.go", "status": "modified", "patch": "@@ -10,2 +10,3 @@\n keep\n+added\n keep"},
		{"filename": "src/gone.go", "status": "removed", "patch": "@@ -1,2 +0,0 @@\n-one\n-two"},
	})
	return &Poller{conf: Config{Repos: []Repo{{Owner: "acme", Name: "web"}}}, rest: rest, home: t.TempDir(),
		gql: fakeBlobs(t, map[string]string{"src/a.go": lines(20)}, queries), now: func() time.Time { return time.Unix(0, 0) }}
}

func submit(t *testing.T, p *Poller, persona, findings string) (string, bool) {
	t.Helper()
	res := p.runSubmitFindings(context.Background(), call(`{"number":42,"persona":"`+persona+`","findings":[`+findings+`]}`), agent.Callbacks{})
	var b strings.Builder
	for _, c := range res.OfToolResult.Content {
		if c.OfText != nil {
			b.WriteString(c.OfText.Text)
		}
	}
	return b.String(), isError(res)
}

func finding(path, lines string) string {
	return `{"severity":"blocking","class":"c","path":"` + path + `"` + lines + `,` + body + `}`
}

func TestSubmitFindingsRefusesALinePastTheEndOfTheFileAndStoresNothing(t *testing.T) {
	var queries int
	p := submitPoller(t, &queries)
	out, failed := submit(t, p, "security", finding("src/a.go", `,"start_line":11`)+","+finding("src/a.go", `,"start_line":15,"end_line":940`))
	if !failed || !strings.Contains(out, "Nothing stored") || !strings.Contains(out, "finding 1 (src/a.go): line 940 is past the end of the file at head, which has 20 lines") {
		t.Fatalf("want a refusal naming finding 1, got failed=%v %q", failed, out)
	}
	if strings.Contains(out, "finding 0") {
		t.Errorf("a valid finding was reported: %q", out)
	}
	if queries != 1 {
		t.Errorf("blobs were fetched in %d queries, want 1", queries)
	}
	if _, err := os.Stat(filepath.Join(p.home, paths.State, "reviews")); err == nil {
		t.Error("a refused call stored something")
	}
}

func TestSubmitFindingsAcceptsAnOffDiffLineThatExistsAtHeadAndReportsIt(t *testing.T) {
	var queries int
	p := submitPoller(t, &queries)
	out, failed := submit(t, p, "security", finding("src/a.go", `,"start_line":11`)+","+finding("src/a.go", `,"start_line":15,"end_line":16`))
	if failed {
		t.Fatalf("refused: %s", out)
	}
	if !strings.Contains(out, "Stored 2 findings from security for acme/web#42 at head "+short(head)) || !strings.Contains(out, "finding 1 (src/a.go:15-16)") {
		t.Errorf("result should count the findings and name the off-diff one: %q", out)
	}
	file, _ := reviewstore.SubmittedPath(p.home, "acme/web", 42, head, "security")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var got reviewstore.Submission
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != 2 || got.Findings[0].OffDiff || !got.Findings[1].OffDiff {
		t.Fatalf("stored %+v", got.Findings)
	}
}

func TestSubmitFindingsStoresUnderTheHeadAndReplacesThePersonasEarlierSubmission(t *testing.T) {
	var queries int
	p := submitPoller(t, &queries)
	if out, failed := submit(t, p, "api-design", finding("src/a.go", `,"start_line":11`)+","+finding("src/a.go", "")); failed {
		t.Fatal(out)
	}
	out, failed := submit(t, p, "api-design", finding("src/a.go", `,"start_line":12`))
	if failed {
		t.Fatal(out)
	}
	want := filepath.Join(p.home, paths.State, "reviews", "acme", "web", "42-submitted", head, "api-design.json")
	if !strings.Contains(out, want) {
		t.Errorf("result names %q, want it to name %s", out, want)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	var got reviewstore.Submission
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Persona != "api-design" || got.Head != head || got.Number != 42 || len(got.Findings) != 1 || got.Findings[0].StartLine != 12 {
		t.Fatalf("the second submission did not replace the first: %+v", got)
	}
}

func TestSubmitFindingsRefusesLinesOnAFileThePRDeletes(t *testing.T) {
	var queries int
	p := submitPoller(t, &queries)
	out, failed := submit(t, p, "security", finding("src/gone.go", `,"start_line":1`))
	if !failed || !strings.Contains(out, "finding 0 (src/gone.go): the PR deletes this file") {
		t.Fatalf("want a refusal for lines on a deleted file, got failed=%v %q", failed, out)
	}
	if out, failed := submit(t, p, "security", finding("src/gone.go", "")); failed {
		t.Errorf("a whole-file finding on a deleted file was refused: %s", out)
	}
}

func TestSubmitFindingsRefusesAPersonaThatIsNotAFileName(t *testing.T) {
	var queries int
	p := submitPoller(t, &queries)
	if _, failed := submit(t, p, "../x", ""); !failed {
		t.Fatal("a persona with a path in it was accepted")
	}
}

func TestOnlyTheReviewerIsOfferedSubmitFindings(t *testing.T) {
	p := &Poller{}
	has := func(handlers map[string]agent.Handler) bool { _, ok := handlers[submitFindingsTool]; return ok }
	if has(p.Handlers()) || has(p.DraftHandlers()) || !has(p.SubmitHandlers()) {
		t.Error("submit_findings is reachable from the main agent's handlers or missing from the reviewer's")
	}
	for _, tool := range append(p.Tools(), p.DraftTools()...) {
		if *tool.GetName() == submitFindingsTool {
			t.Error("submit_findings is in the main agent's tool list")
		}
	}
}
