package prs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/reviewstore"
)

func reviewPoller(t *testing.T) *Poller {
	rest, _ := fakeREST(t, []map[string]any{
		{"filename": "src/a.go", "status": "modified", "patch": "@@ -10,2 +10,3 @@\n keep\n+added\n keep"},
	})
	return &Poller{conf: Config{Repos: []Repo{{Owner: "acme", Name: "web"}}}, rest: rest, home: t.TempDir(),
		now: func() time.Time { return time.Unix(0, 0) }}
}

func resultText(t *testing.T, p *Poller, input string) (string, bool) {
	t.Helper()
	res := p.runReviewLoad(context.Background(), call(input), agent.Callbacks{})
	var b strings.Builder
	for _, c := range res.OfToolResult.Content {
		if c.OfText != nil {
			b.WriteString(c.OfText.Text)
		}
	}
	return b.String(), isError(res)
}

func TestReviewLoadStoresNothingWhileAnyFindingIsInvalid(t *testing.T) {
	p := reviewPoller(t)
	out, failed := resultText(t, p, `{"number":42,"verdict":"request changes","summary":"s","findings":[
		{"severity":"blocking","class":"c","path":"src/a.go","start_line":11,`+body+`},
		{"severity":"blocking","class":"c","path":"a.go","start_line":11,`+body+`}]}`)
	if !failed || !strings.Contains(out, "Nothing stored") || !strings.Contains(out, "finding 1 (a.go)") {
		t.Fatalf("want a refusal naming finding 1, got failed=%v %q", failed, out)
	}
	if _, ok, _ := reviewstore.Load(p.home, "acme/web", 42); ok {
		t.Fatal("a refused call stored a review")
	}
}

func TestReviewLoadPinsTheReviewToTheHeadAndReportsOffDiffFindings(t *testing.T) {
	p := reviewPoller(t)
	var heard []reviewstore.Review
	p.OnReview = func(r reviewstore.Review) { heard = append(heard, r) }
	out, failed := resultText(t, p, `{"number":42,"verdict":"request changes","summary":"s","findings":[
		{"severity":"blocking","class":"c","path":"src/a.go","start_line":11,`+body+`},
		{"severity":"non-blocking","class":"c","path":"src/a.go","start_line":90,"end_line":95,`+body+`}]}`)
	if failed {
		t.Fatalf("refused: %s", out)
	}
	if !strings.Contains(out, "2 findings") || !strings.Contains(out, "finding 1 (src/a.go:90-95)") {
		t.Errorf("result should count the findings and name the off-diff one: %q", out)
	}
	r, ok, err := reviewstore.Load(p.home, "acme/web", 42)
	if err != nil || !ok || r.Head != "0123456789abcdef" || r.Findings[0].OffDiff || !r.Findings[1].OffDiff {
		t.Fatalf("stored %+v ok=%v err=%v", r, ok, err)
	}
	if len(heard) != 1 || heard[0].Number != 42 {
		t.Errorf("OnReview heard %+v", heard)
	}
}

func TestReviewLoadRefusesACallerSettingOffDiff(t *testing.T) {
	p := reviewPoller(t)
	if _, failed := resultText(t, p, `{"number":42,"verdict":"approve","summary":"s","findings":[
		{"severity":"blocking","class":"c","path":"src/a.go",`+body+`,"off_diff":true}]}`); !failed {
		t.Fatal("a caller-supplied off_diff was accepted")
	}
}

func TestReviewLoadRefusesAFindingGivenAsOneText(t *testing.T) {
	p := reviewPoller(t)
	out, failed := resultText(t, p, `{"number":42,"verdict":"approve","summary":"s","findings":[
		{"severity":"blocking","class":"c","path":"src/a.go",`+body+`,"text":"everything in one block"}]}`)
	if !failed || !strings.Contains(out, "problem, impact, evidence and fix") {
		t.Fatalf("a finding with text should be refused and pointed at the four fields; failed=%v %q", failed, out)
	}
}

func storePrior(t *testing.T, p *Poller) {
	t.Helper()
	err := reviewstore.Save(p.home, reviewstore.Review{Repo: "acme/web", Number: 42, Head: "aaaaaaaaaaaaaaaa", Verdict: "approve", Findings: []reviewstore.Finding{
		{Severity: "blocking", Class: "c", Path: "src/a.go", StartLine: 11, Text: "dismissed", Dismissed: true},
		{Severity: "blocking", Class: "c", Path: "src/a.go", StartLine: 11, Text: "discussed", Thread: []reviewstore.Message{{Role: "user", Text: "why?"}, {Role: "assistant", Text: "because"}, {Role: "user", Text: "ok"}}},
		{Severity: "blocking", Class: "c", Path: "src/a.go", StartLine: 11, Text: "also dismissed", Dismissed: true},
		{Severity: "blocking", Class: "c", Path: "src/a.go", StartLine: 11, Text: "untouched"},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

const body = `"problem":"p","impact":"i","evidence":"e","fix":"f"`

const continuingFindings = `{"number":42,"verdict":"request changes","summary":"s","findings":[
	{"severity":"blocking","class":"c","path":"src/a.go","start_line":11,` + body + `,"continues":1},
	{"severity":"blocking","class":"c","path":"src/a.go","start_line":11,` + body + `},
	{"severity":"non-blocking","class":"c","path":"src/a.go","start_line":11,` + body + `,"continues":0},
	{"severity":"non-blocking","class":"c","path":"src/a.go","start_line":11,` + body + `,"continues":3}]}`

func TestReviewLoadCarriesDismissalsAndThreadsOntoContinuedFindings(t *testing.T) {
	p := reviewPoller(t)
	storePrior(t, p)
	out, failed := resultText(t, p, continuingFindings)
	if failed {
		t.Fatalf("refused: %s", out)
	}
	for _, want := range []string{
		"Carried from the earlier review: finding 0 (thread of 3 messages), finding 2 (dismissed).",
		"Not continued, so their dismissal or thread is gone: earlier finding 2 (src/a.go, dismissed).",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	r, _, err := reviewstore.Load(p.home, "acme/web", 42)
	if err != nil || r.Head != "0123456789abcdef" || len(r.Findings) != 4 {
		t.Fatalf("stored %+v err=%v", r, err)
	}
	if f := r.Findings[0]; f.Dismissed || len(f.Thread) != 3 || f.Thread[0].Text != "why?" {
		t.Errorf("finding 0 = %+v, want the earlier thread and no dismissal", f)
	}
	if f := r.Findings[1]; f.Dismissed || len(f.Thread) != 0 {
		t.Errorf("a new finding picked up %+v", f)
	}
	if f := r.Findings[2]; !f.Dismissed || len(f.Thread) != 0 {
		t.Errorf("finding 2 = %+v, want dismissed", f)
	}
	if f := r.Findings[3]; f.Dismissed || len(f.Thread) != 0 {
		t.Errorf("continuing an untouched finding set %+v", f)
	}
}

func TestReviewLoadRefusesContinuesThatPointsNowhere(t *testing.T) {
	finding := func(c string) string {
		return `{"number":42,"verdict":"approve","summary":"s","findings":[
			{"severity":"blocking","class":"c","path":"src/a.go",` + body + `,"continues":` + c + `},
			{"severity":"blocking","class":"c","path":"src/a.go",` + body + `,"continues":1}]}`
	}
	for _, c := range []struct {
		name, input, want string
		prior             bool
	}{
		{"no earlier review", finding("0"), "finding 0: continues 0, but the PR has no earlier stored review", false},
		{"past the last finding", finding("4"), "finding 0: continues 4, but the earlier review has 4 findings (0 to 3)", true},
		{"negative", finding("-1"), "finding 0: continues -1, but the earlier review has 4 findings (0 to 3)", true},
		{"same finding twice", finding("1"), "finding 1: continues 1, which finding 0 already continues", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := reviewPoller(t)
			if c.prior {
				storePrior(t, p)
			}
			out, failed := resultText(t, p, c.input)
			if !failed || !strings.Contains(out, "Nothing stored") || !strings.Contains(out, c.want) {
				t.Fatalf("want a refusal with %q, got failed=%v %q", c.want, failed, out)
			}
			r, ok, _ := reviewstore.Load(p.home, "acme/web", 42)
			if ok != c.prior || (ok && r.Head != "aaaaaaaaaaaaaaaa") {
				t.Errorf("a refused call changed the store: %+v ok=%v", r, ok)
			}
		})
	}
}

func TestReviewLoadWithoutContinuesNamesWhatItDropped(t *testing.T) {
	p := reviewPoller(t)
	storePrior(t, p)
	out, failed := resultText(t, p, `{"number":42,"verdict":"approve","summary":"s","findings":[]}`)
	if failed || strings.Contains(out, "Carried") ||
		!strings.Contains(out, "earlier finding 0 (src/a.go, dismissed), earlier finding 1 (src/a.go, thread of 3 messages), earlier finding 2 (src/a.go, dismissed)") {
		t.Errorf("failed=%v %q", failed, out)
	}
}
