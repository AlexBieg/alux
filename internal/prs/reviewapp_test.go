package prs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const envComment = `Preview apps are ready:

* **Web**: https://web-4242-brave-otter.test.example.test
* **Portal**: https://portal-4242-brave-otter.test.example.test
* **Docs**: https://docs-4242-brave-otter.test.example.test
* **Api**: https://api-4242-brave-otter.test.example.test
* **Status**: https://status-4242-brave-otter.test.example.test/signup/FOOBAR
`

const (
	healthyCheck = `{"__typename": "CheckRun", "name": "Deploy Preview / Preview Ready", "status": "COMPLETED", "conclusion": "SUCCESS"}`
	pendingCheck = `{"__typename": "CheckRun", "name": "Deploy Preview / Preview Ready", "status": "IN_PROGRESS", "conclusion": null}`
	failedCheck  = `{"__typename": "CheckRun", "name": "Deploy Preview / Preview Ready", "status": "COMPLETED", "conclusion": "FAILURE"}`
	otherCheck   = `{"__typename": "CheckRun", "name": "unit", "status": "COMPLETED", "conclusion": "SUCCESS"}`
)

var testEnv = ReviewAppConfig{Label: "deploy-preview", Bot: "preview-bot", HostSuffix: ".test.example.test", HealthCheck: "Preview Ready"}

type envFakeComment struct{ author, body string }

func envResponse(labels []string, comments []envFakeComment, checks ...string) string {
	var ls, cs []string
	for _, l := range labels {
		ls = append(ls, fmt.Sprintf(`{"name": %q}`, l))
	}
	for _, c := range comments {
		body, _ := json.Marshal(c.body)
		cs = append(cs, fmt.Sprintf(`{"author": {"login": %q}, "body": %s}`, c.author, body))
	}
	return fmt.Sprintf(`{"data": {"repository": {"pullRequest": {
  "labels": {"nodes": [%s]},
  "comments": {"nodes": [%s]},
  "commits": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [%s]}}}}]}
}}}}`, strings.Join(ls, ","), strings.Join(cs, ","), strings.Join(checks, ","))
}

func fetchEnv(t *testing.T, response string) (ReviewApp, error) {
	t.Helper()
	return FetchReviewApp(context.Background(), func(context.Context, string) ([]byte, error) { return []byte(response), nil }, testEnv, "acme", "web", 7)
}

func TestReviewAppReadyParsesOriginsAndSlug(t *testing.T) {
	var asked string
	app, err := FetchReviewApp(context.Background(), func(_ context.Context, q string) ([]byte, error) {
		asked = q
		return []byte(envResponse([]string{"core", testEnv.Label}, []envFakeComment{{"preview-bot", envComment}}, otherCheck, healthyCheck)), nil
	}, testEnv, "acme", "web", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked, `repository(owner: "acme", name: "web") { pullRequest(number: 7)`) {
		t.Errorf("query not scoped to the PR:\n%s", asked)
	}
	if app.State != EnvReady || app.Slug != "4242-brave-otter" {
		t.Errorf("state=%q slug=%q", app.State, app.Slug)
	}
	want := map[string]string{
		"web":    "https://web-4242-brave-otter.test.example.test",
		"portal": "https://portal-4242-brave-otter.test.example.test",
		"docs":   "https://docs-4242-brave-otter.test.example.test",
		"api":    "https://api-4242-brave-otter.test.example.test",
		"status": "https://status-4242-brave-otter.test.example.test",
	}
	if len(app.Apps) != len(want) {
		t.Errorf("apps = %v", app.Apps)
	}
	for k, origin := range want {
		if app.Apps[k] != origin {
			t.Errorf("apps[%q] = %q, want %q", k, app.Apps[k], origin)
		}
	}
}

func TestReviewAppIgnoresCommentsItCannotTrust(t *testing.T) {
	offHost := strings.NewReplacer(".test.example.test", ".example.com").Replace(envComment)
	plain := strings.ReplaceAll(envComment, "https://", "http://")
	cases := map[string][]envFakeComment{
		"another author":  {{"someone-else", envComment}},
		"foreign host":    {{"preview-bot", offHost}},
		"not https":       {{"preview-bot", plain}},
		"lookalike login": {{"preview-bot-2", envComment}},
	}
	for name, comments := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := fetchEnv(t, envResponse([]string{testEnv.Label}, comments, healthyCheck))
			if err == nil || !strings.Contains(err.Error(), "comment") {
				t.Errorf("err = %v; want the missing-comment error", err)
			}
		})
	}
}

// Another repo's label, bot, host and check are read from its config, not assumed.
func TestReviewAppReadsItsLabelBotHostAndCheckFromConfig(t *testing.T) {
	conf := ReviewAppConfig{Label: "preview", Bot: "site-bot", HostSuffix: ".example.com", HealthCheck: "Site Healthy"}
	comment := strings.NewReplacer(".test.example.test", ".example.com").Replace(envComment)
	check := strings.ReplaceAll(healthyCheck, "Preview Ready", "Site Healthy")
	fetch := func(response string) (ReviewApp, error) {
		return FetchReviewApp(context.Background(), func(context.Context, string) ([]byte, error) { return []byte(response), nil }, conf, "acme", "web", 7)
	}

	app, err := fetch(envResponse([]string{"preview"}, []envFakeComment{{"site-bot", comment}}, check))
	if err != nil || app.State != EnvReady || app.Label != "preview" || app.Apps["web"] != "https://web-4242-brave-otter.example.com" {
		t.Errorf("app=%+v err=%v", app, err)
	}
	if app, err := fetch(envResponse([]string{testEnv.Label}, []envFakeComment{{"site-bot", comment}}, check)); err != nil || app.State != EnvNone {
		t.Errorf("another repo's label counted: app=%+v err=%v", app, err)
	}
	if app, err := fetch(envResponse([]string{"preview"}, []envFakeComment{{"site-bot", comment}}, healthyCheck)); err != nil || app.State != EnvDeploying {
		t.Errorf("another repo's health check counted: app=%+v err=%v", app, err)
	}
	if _, err := fetch(envResponse([]string{"preview"}, []envFakeComment{{"site-bot", envComment}}, check)); err == nil {
		t.Error("URLs on another repo's host were accepted")
	}
}

func TestReviewAppUsesTheLatestBotComment(t *testing.T) {
	older := strings.ReplaceAll(envComment, "4242-brave-otter", "1111-old-name")
	app, err := fetchEnv(t, envResponse([]string{testEnv.Label}, []envFakeComment{
		{"preview-bot", older}, {"kim", "looks good"}, {"preview-bot", envComment},
	}, healthyCheck))
	if err != nil {
		t.Fatal(err)
	}
	if app.Slug != "4242-brave-otter" {
		t.Errorf("slug = %q, want the newest comment's", app.Slug)
	}
}

func TestReviewAppStates(t *testing.T) {
	bot := []envFakeComment{{"preview-bot", envComment}}
	cases := []struct {
		name     string
		response string
		want     EnvState
	}{
		{"no label", envResponse([]string{"core"}, bot, healthyCheck), EnvNone},
		{"check still running", envResponse([]string{testEnv.Label}, bot, pendingCheck), EnvDeploying},
		{"check not reported yet", envResponse([]string{testEnv.Label}, bot, otherCheck), EnvDeploying},
		{"a retry running beside a failure", envResponse([]string{testEnv.Label}, bot, failedCheck, pendingCheck), EnvDeploying},
		{"check failed", envResponse([]string{testEnv.Label}, bot, failedCheck), EnvFailed},
		{"a retry passed after a failure", envResponse([]string{testEnv.Label}, bot, failedCheck, healthyCheck), EnvReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, err := fetchEnv(t, c.response)
			if err != nil {
				t.Fatal(err)
			}
			if app.State != c.want {
				t.Errorf("state = %q, want %q", app.State, c.want)
			}
			if c.want != EnvReady && len(app.Apps) != 0 {
				t.Errorf("apps = %v on a %s environment", app.Apps, c.want)
			}
		})
	}
}

func TestReviewAppReadsTheHealthCheckAndBotCommentPastTheFirstPage(t *testing.T) {
	bot, _ := json.Marshal(envComment)
	first := `{"data": {"repository": {"pullRequest": {
  "labels": {"nodes": [{"name": "deploy-preview"}]},
  "comments": {"pageInfo": {"hasNextPage": true, "endCursor": "m1"}, "nodes": [{"author": {"login": "kim"}, "body": "hi"}]},
  "commits": {"nodes": [{"commit": {"oid": "head1", "statusCheckRollup": {"contexts": {
    "pageInfo": {"hasNextPage": true, "endCursor": "k1"}, "nodes": [` + otherCheck + `]}}}}]}
}}}}`
	var asked []string
	app, err := FetchReviewApp(context.Background(), func(_ context.Context, q string) ([]byte, error) {
		asked = append(asked, q)
		switch {
		case strings.Contains(q, `object(oid: "head1")`) && strings.Contains(q, `after: "k1"`):
			return []byte(`{"data": {"repository": {"object": {"statusCheckRollup": {"contexts": {"pageInfo": {"hasNextPage": false}, "nodes": [` + healthyCheck + `]}}}}}}`), nil
		case strings.Contains(q, `comments(first: 100, after: "m1")`):
			return []byte(`{"data": {"repository": {"pullRequest": {"comments": {"pageInfo": {"hasNextPage": false}, "nodes": [{"author": {"login": "preview-bot"}, "body": ` + string(bot) + `}]}}}}}`), nil
		case len(asked) == 1:
			return []byte(first), nil
		}
		return nil, fmt.Errorf("unexpected query:\n%s", q)
	}, testEnv, "acme", "web", 7)
	if err != nil {
		t.Fatal(err)
	}
	if app.State != EnvReady || app.Slug != "4242-brave-otter" || len(asked) != 3 {
		t.Errorf("state=%q slug=%q after %d queries", app.State, app.Slug, len(asked))
	}
}

func TestReviewAppHealthyWithoutBotCommentIsAnError(t *testing.T) {
	_, err := fetchEnv(t, envResponse([]string{testEnv.Label}, nil, healthyCheck))
	if err == nil || !strings.Contains(err.Error(), "deploy bot's comment") {
		t.Errorf("err = %v", err)
	}
}

func TestReviewAppPick(t *testing.T) {
	conf := ReviewAppConfig{AppsDir: "apps/", Skip: []string{"api"}, Rank: []string{"web", "portal", "admin"}}
	app := ReviewApp{State: EnvReady, Config: conf, Apps: map[string]string{
		"web":    "https://web-1-a-b.test.example.test",
		"portal": "https://portal-1-a-b.test.example.test",
		"admin":  "https://admin-1-a-b.test.example.test",
		"api":    "https://api-1-a-b.test.example.test",
	}}
	cases := []struct {
		name  string
		files []string
		app   ReviewApp
		want  string
	}{
		{"web files win", []string{"apps/web/a.ts", "apps/web/b.ts", "apps/portal/c.ts"}, app, "web"},
		{"portal beats web on count", []string{"apps/web/a.ts", "apps/portal/b.ts", "apps/portal/c.ts"}, app, "portal"},
		{"tie goes to web, portal, admin in that order", []string{"apps/admin/a.ts", "apps/portal/b.ts"}, app, "portal"},
		{"backend only falls back to web", []string{"server/models.py", "apps/api/x.ts"}, app, "web"},
		{"shared packages only falls back to web", []string{"packages/ui/button.tsx"}, app, "web"},
		{"a dir with no app is not counted", []string{"apps/unknown/a.ts", "apps/admin/b.ts"}, app, "admin"},
		{"a file directly under apps is not an app", []string{"apps/README.md"}, app, "web"},
		{"nothing fits", []string{"api/a.py"}, ReviewApp{Config: conf, Apps: map[string]string{"api": "https://api-1-a-b.test.example.test"}}, ""},
		{"unlisted apps tie alphabetically", []string{"apps/zeta/a.ts", "apps/beta/b.ts"}, ReviewApp{Config: conf, Apps: map[string]string{
			"zeta": "https://zeta-1-a-b.test.example.test", "beta": "https://beta-1-a-b.test.example.test"}}, "beta"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, origin := c.app.Pick(c.files)
			if key != c.want || origin != c.app.Apps[c.want] {
				t.Errorf("Pick = %q, %q; want %q", key, origin, c.want)
			}
		})
	}
}

func TestAnEmptyReviewAppConfigPutsNoAppFirstAndPicksNothing(t *testing.T) {
	apps := map[string]string{"api": "https://api-1-a.test.example", "admin": "https://admin-2-b.test.example"}
	if got := envSlug(apps, ""); got != "2-b" {
		t.Errorf("envSlug with no slug_app = %q, want the alphabetically first app's", got)
	}
	if got := envSlug(apps, "api"); got != "1-a" {
		t.Errorf("envSlug with slug_app = %q, want that app's", got)
	}
	app := ReviewApp{Apps: map[string]string{"web": "https://web-1-a.test.example"}}
	if key, origin := app.Pick([]string{"apps/web/x.ts"}); key != "" || origin != "" {
		t.Errorf("Pick with no apps_dir = %q, %q; want nothing", key, origin)
	}
}

func TestPollerReviewAppNeedsTheRepoToOptIn(t *testing.T) {
	gql := func(context.Context, string) ([]byte, error) {
		return []byte(envResponse([]string{"core"}, nil)), nil
	}
	p := &Poller{conf: Config{Repos: []Repo{{Owner: "acme", Name: "web"}, {Owner: "acme", Name: "api", ReviewApp: &testEnv}}}, gql: gql}
	_, err := p.ReviewApp(context.Background(), "acme/web", 7)
	if err == nil || !strings.Contains(err.Error(), `acme/web has no review apps; set "review_app" under prs.repos.acme/web`) {
		t.Errorf("err = %v", err)
	}
	app, err := p.ReviewApp(context.Background(), "acme/api", 7)
	if err != nil || app.State != EnvNone || app.Label != testEnv.Label {
		t.Errorf("app=%+v err=%v", app, err)
	}
}

func TestPollerAddLabelPostsTheLabel(t *testing.T) {
	f := &fakeGitHub{}
	f.on("POST repos/o/r/issues/12/labels", `[]`)
	if err := poller(f).AddLabel(context.Background(), "", 12, testEnv.Label); err != nil {
		t.Fatal(err)
	}
	got := f.writes()
	if len(got) != 1 || got[0].method != "POST" || got[0].path != "repos/o/r/issues/12/labels" {
		t.Fatalf("sent %+v", got)
	}
	labels, _ := got[0].body["labels"].([]any)
	if len(labels) != 1 || labels[0] != testEnv.Label {
		t.Errorf("body = %v", got[0].body)
	}
}

func TestReviewAppNeedsTheHeadingOnlyWhenOneIsConfigured(t *testing.T) {
	bare := []envFakeComment{{testEnv.Bot, "* **Web**: https://web-1-a-b.test.example.test"}}
	fetch := func(conf ReviewAppConfig, comments []envFakeComment) (ReviewApp, error) {
		response := envResponse([]string{testEnv.Label}, comments, healthyCheck)
		return FetchReviewApp(context.Background(), func(context.Context, string) ([]byte, error) { return []byte(response), nil }, conf, "acme", "web", 7)
	}
	if app, err := fetch(testEnv, bare); err != nil || app.State != EnvReady {
		t.Errorf("no heading configured: app=%+v err=%v; want any bot comment with app lines to count", app, err)
	}
	conf := testEnv
	conf.Heading, _, _ = strings.Cut(envComment, "\n")
	if app, err := fetch(conf, []envFakeComment{{testEnv.Bot, envComment}}); err != nil || app.State != EnvReady {
		t.Errorf("heading present: app=%+v err=%v", app, err)
	}
	if _, err := fetch(conf, bare); err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Errorf("heading missing: err = %v; want the missing-comment error", err)
	}
}
