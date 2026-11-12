package prs

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseOffWithoutBlock(t *testing.T) {
	_, ok, err := Parse([]byte(`{"vault": "/x"}`))
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v, want off and no error", ok, err)
	}
}

func TestParseDefaultsForBareRepo(t *testing.T) {
	conf, ok, err := Parse([]byte(`{"prs": {"repos": {"acme/web": {}}}}`))
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if conf.Poll != defaultPoll {
		t.Errorf("poll = %v", conf.Poll)
	}
	r := conf.Repos[0]
	if r.Owner != "acme" || r.Name != "web" || r.Review.Queries[0].Search != "review-requested:@me" ||
		r.Mine.Queries[0].Search != "author:@me" || r.Flags[0].Kind != FlagPendingReview {
		t.Errorf("defaults not applied: %+v", r)
	}
}

func TestParseRefusesWhatWouldFilterSilently(t *testing.T) {
	cases := map[string]string{
		"misspelt exclude":      `{"kinds": {"review": {"queries": ["label:x"], "exclude": ["drafts"]}}}`,
		"misspelt flag":         `{"flags": ["pending_review"]}`,
		"unknown kind":          `{"kinds": {"reviews": {"queries": ["label:x"]}}}`,
		"unknown field":         `{"kinds": {"review": {"queries": ["label:x"], "exlude": []}}}`,
		"team rule, no team":    `{"kinds": {"review": {"queries": ["label:x"], "exclude": ["approved_by_team"]}}}`,
		"repo qualifier":        `{"kinds": {"review": {"queries": ["repo:other/x label:y"]}}}`,
		"unless_label alone":    `{"kinds": {"review": {"queries": ["label:x"], "exclude": [{"unless_label": "ready"}]}}}`,
		"bad regexp":            `{"kinds": {"review": {"queries": [{"search": "x", "title_matches": "("}]}}}`,
		"flag missing argument": `{"flags": ["label_on_red"]}`,
	}
	for name, repo := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := Parse([]byte(`{"prs": {"repos": {"acme/web": ` + repo + `}}}`))
			if err == nil {
				t.Fatal("parsed; want an error")
			}
		})
	}
}

func TestParseReviewApps(t *testing.T) {
	conf, _, err := Parse([]byte(`{"prs": {"repos": {"acme/web": {"review_app": {"label": "preview", "bot": "preview-bot",
		"host_suffix": ".Example.com", "health_check": "Preview Ready"}}, "acme/api": {}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := ReviewAppConfig{Label: "preview", Bot: "preview-bot", HostSuffix: ".example.com", HealthCheck: "Preview Ready"}
	if api, web := conf.Repos[0], conf.Repos[1]; api.ReviewApp != nil || web.ReviewApp == nil || !reflect.DeepEqual(*web.ReviewApp, want) {
		t.Errorf("review_app not read per repo: api=%+v web=%+v", api.ReviewApp, web.ReviewApp)
	}
	for name, repo := range map[string]string{
		"the old bool":   `{"review_apps": true}`,
		"a missing host": `{"review_app": {"label": "l", "bot": "b", "health_check": "c"}}`,
		"an unknown key": `{"review_app": {"label": "l", "bot": "b", "host_suffix": ".x", "health_check": "c", "headline": "h"}}`,
	} {
		if _, _, err := Parse([]byte(`{"prs": {"repos": {"acme/web": ` + repo + `}}}`)); err == nil {
			t.Errorf("%s parsed; want an error", name)
		}
	}
}

func TestParseReadyLabelAndReviewBotPerRepo(t *testing.T) {
	conf, _, err := Parse([]byte(`{"prs": {"repos": {"acme/web": {"ready_label": "ship", "bugbot": true}, "acme/api": {}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	api, web := conf.Repos[0], conf.Repos[1]
	if api.ReadyLabel != "" || api.ReviewBot != nil || web.ReadyLabel != "ship" || web.ReviewBot == nil {
		t.Fatalf("not read per repo: api=%+v web=%+v", api, web)
	}
	if *web.ReviewBot != (ReviewBot{Trigger: "bugbot run", Login: "cursor"}) {
		t.Errorf("bugbot: true = %+v", *web.ReviewBot)
	}
}

func TestParseBugbotTakesATriggerAndLogin(t *testing.T) {
	conf, _, err := Parse([]byte(`{"prs": {"repos": {"acme/web": {"bugbot": {"trigger": "/review", "login": "reviewer"}}, "acme/api": {"bugbot": false}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if api, web := conf.Repos[0], conf.Repos[1]; api.ReviewBot != nil || web.ReviewBot == nil || *web.ReviewBot != (ReviewBot{Trigger: "/review", Login: "reviewer"}) {
		t.Errorf("api=%+v web=%+v", api.ReviewBot, web.ReviewBot)
	}
	for _, bad := range []string{`{"trigger": "x"}`, `{"trigger": "x", "login": "y", "extra": 1}`, `"yes"`} {
		if _, _, err := Parse([]byte(`{"prs": {"repos": {"acme/web": {"bugbot": ` + bad + `}}}}`)); err == nil {
			t.Errorf("bugbot %s parsed; want an error", bad)
		}
	}
}

func TestParseFullConfig(t *testing.T) {
	conf, _, err := Parse([]byte(`{"prs": {"poll": "2m", "repos": {"acme/web": {
		"team": "acme/core",
		"kinds": {"review": {
			"queries": ["label:core", {"search": "CORE in:title", "title_matches": "\\bCORE-\\d+"}],
			"exclude": ["draft", "approved_by_team", {"author": "pat", "body_matches": "WIP", "unless_label": "core"}]
		}},
		"flags": ["my_pending_review", "label_on_red:merge-queue"],
		"merge_label": "merge-queue"
	}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := conf.Repos[0]
	if conf.Poll != 2*time.Minute || len(r.Review.Queries) != 2 || len(r.Review.Exclude) != 3 || len(r.Flags) != 2 || r.MergeLabel != "merge-queue" {
		t.Fatalf("parsed %+v", conf)
	}
	if !r.Review.Queries[1].TitleMatches.MatchString("[CORE-12] x") || r.Flags[1].Label != "merge-queue" {
		t.Errorf("query or flag argument lost: %+v", r)
	}
	if !strings.HasPrefix(r.Mine.Queries[0].Search, "author:") {
		t.Errorf("mine should keep its default when only review is configured")
	}
}
