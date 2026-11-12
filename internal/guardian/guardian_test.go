package guardian

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// server answers every command with probs, one per category in rubric order.
func server(t *testing.T, probs []float64, up *atomic.Bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if up != nil && !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			State     map[string]string `json:"state"`
			Questions map[string]struct {
				Type     string   `json:"type"`
				Criteria []string `json:"criteria"`
			} `json:"questions"`
		}
		if r.URL.Path != "/v1/systemone" || json.NewDecoder(r.Body).Decode(&req) != nil || req.State["command"] == "" ||
			req.Questions["does"].Type != "score" || len(req.Questions["does"].Criteria) != len(Categories) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		parts := make([]string, len(probs))
		for i, p := range probs {
			parts[i] = fmt.Sprintf("%q: %g", fmt.Sprint(i), p)
		}
		fmt.Fprintf(w, `{"answers": {"does": {"type": "score", "score": 1, "probabilities": {%s}}}}`, strings.Join(parts, ", "))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

var (
	coder = Criteria{Agent: "coder", Allowed: []Category{Read, Build, Edit}, Threshold: 0.32}
	main  = Criteria{Agent: "main", Allowed: []Category{Read, Build, Edit, Record}, Threshold: 0.26}
)

// read build edit record rewrite destroy network system
var commit = []float64{0.02, 0.05, 0.03, 0.86, 0.02, 0.0, 0.01, 0.01}

// One answer, two agents: the same commit is ordinary work for one and outside the other's.
func TestTheSameAnswerIsJudgedByEachAgentsOwnCriteria(t *testing.T) {
	c := New(Conf{URL: server(t, commit, nil), Model: "d3:2b"}, nil)
	forMain := c.Classify(context.Background(), "jj commit -m x", main)
	forCoder := c.Classify(context.Background(), "jj commit -m x", coder)
	if forMain.Top != Record || forCoder.Top != Record {
		t.Fatalf("top category: main %q, coder %q, want record", forMain.Top, forCoder.Top)
	}
	if forMain.Flagged {
		t.Errorf("a commit stopped the main agent: danger %.2f", forMain.Danger)
	}
	if !forCoder.Flagged {
		t.Errorf("a commit did not stop the coder: danger %.2f", forCoder.Danger)
	}
}

// The probabilities are read one by one, so a safe step cannot average a dangerous one away.
func TestDangerIsTheMassOutsideTheAllowedCategories(t *testing.T) {
	split := []float64{0.0, 0.5, 0.0, 0.0, 0.0, 0.4, 0.1, 0.0}
	v := New(Conf{URL: server(t, split, nil), Model: "m"}, nil).Classify(context.Background(), "make && rm -rf ~/x", main)
	if v.Danger < 0.49 || v.Danger > 0.51 || !v.Flagged {
		t.Errorf("danger %.2f flagged %v, want 0.50 and flagged", v.Danger, v.Flagged)
	}
}

func TestCriteriaMustSayWhatTheAgentDoes(t *testing.T) {
	for _, bad := range []Criteria{
		{Allowed: []Category{Read}, Threshold: 0.5},
		{Agent: "x", Threshold: 0.5},
		{Agent: "x", Allowed: []Category{"browse"}, Threshold: 0.5},
		{Agent: "x", Allowed: []Category{Read}},
		{Agent: "x", Allowed: []Category{Read}, Threshold: 1},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := coder.Validate(); err != nil {
		t.Errorf("rejected the coder's criteria: %v", err)
	}
}

// A stopped server is one notice, not one per command, but going down again after recovering is news.
func TestAnOutageIsReportedOnce(t *testing.T) {
	var up atomic.Bool
	var notices int
	c := New(Conf{URL: server(t, commit, &up), Model: "m"}, func(string) { notices++ })
	for range 3 {
		if v := c.Classify(context.Background(), "ls", main); v.Known() {
			t.Fatalf("a failed request produced a verdict: %+v", v)
		}
	}
	if notices != 1 {
		t.Fatalf("%d notices for one outage, want 1", notices)
	}
	up.Store(true)
	if !c.Classify(context.Background(), "ls", main).Known() {
		t.Fatal("no verdict once the server came back")
	}
	up.Store(false)
	c.Classify(context.Background(), "ls", main)
	if notices != 2 {
		t.Errorf("%d notices after a second outage, want 2", notices)
	}
}

func TestAGateWithNoClientJudgesNothing(t *testing.T) {
	if (Gate{Criteria: main}).Screen(context.Background(), "ls").Known() {
		t.Error("a gate with no client produced a verdict")
	}
}

func logLine(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var e map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &e); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	return e
}

func TestAnOverriddenStopIsLoggedAsAMisclassification(t *testing.T) {
	log := filepath.Join(t.TempDir(), "verdicts.jsonl")
	g := Gate{Client: New(Conf{URL: server(t, commit, nil), Model: "m", Log: log}, nil), Criteria: coder}
	v := g.Screen(context.Background(), "jj commit -m x")
	g.Record("jj commit -m x", v, "approved")
	e := logLine(t, log)
	if e["agent"] != "coder" || e["human"] != "safe" || e["misclassified"] != true || e["top"] != "record" {
		t.Errorf("override not logged as a misclassification: %v", e)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(log), "questions-"+QuestionsID()+".json")); err != nil {
		t.Errorf("the questions behind the verdict were not saved: %v", err)
	}
}

// suggestServer answers the tool question with probs, rejecting criteria that are not an object, as the model does.
func suggestServer(t *testing.T, probs map[string]float64, hits *atomic.Int32, body *[]byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		data, _ := io.ReadAll(r.Body)
		if body != nil {
			*body = data
		}
		var req struct {
			Questions map[string]struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		if json.Unmarshal(data, &req) != nil || req.Questions["suggest"].Type != "choice" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		answer, _ := json.Marshal(map[string]any{"answers": map[string]any{"suggest": map[string]any{
			"type": "choice", "probabilities": probs,
		}}})
		w.Write(answer)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

var allTools = []string{"view", "grep", "glob"}

func keyOrder(t *testing.T, data []byte, keys ...string) {
	t.Helper()
	last := -1
	for _, k := range keys {
		i := bytes.Index(data, []byte(`"`+k+`":`))
		if i <= last {
			t.Fatalf("%s is not after the keys before it in %s", k, data)
		}
		last = i
	}
}

func TestTheToolMenuHoldsOnlyTheAgentsToolsInOrderWithBashLast(t *testing.T) {
	var hits atomic.Int32
	var body []byte
	c := New(Conf{URL: suggestServer(t, map[string]float64{"view": 0.1, "glob": 0.8, "bash": 0.1}, &hits, &body), Model: "m"}, nil)
	s := c.Suggest(context.Background(), "find . -name '*.go'", []string{"glob", "view", "bash"})
	if !slices.Equal(s.Menu, []string{"view", "glob", "bash"}) || !slices.Equal(s.Tools, []string{"glob"}) {
		t.Fatalf("menu %v tools %v, want [view glob bash] and [glob]", s.Menu, s.Tools)
	}
	keyOrder(t, body, "view", "glob", "bash")
	if bytes.Contains(body, []byte(`"grep":`)) {
		t.Errorf("the request offers grep, which the agent lacks: %s", body)
	}
	all, err := json.Marshal(suggestQuestions(suggestMenu(allTools)))
	if err != nil {
		t.Fatal(err)
	}
	keyOrder(t, all, "view", "grep", "glob", "bash")
}

func TestNoFileToolsMeansNoQuestion(t *testing.T) {
	var hits atomic.Int32
	c := New(Conf{URL: suggestServer(t, map[string]float64{"bash": 1}, &hits, nil), Model: "m"}, nil)
	if s := c.Suggest(context.Background(), "go test", []string{"bash"}); s.Known() || hits.Load() != 0 {
		t.Errorf("asked %d times with only bash, got %+v", hits.Load(), s)
	}
}

func TestASecondToolIsSuggestedOnlyWhenItClearsTheThresholdAlone(t *testing.T) {
	for _, tc := range []struct {
		view, grep, bash float64
		want             []string
	}{
		{0.88, 0.04, 0.07, []string{"view"}},
		{0.45, 0.44, 0.11, []string{"view", "grep"}},
		{0.20, 0.50, 0.30, []string{"grep"}},
		{0.30, 0.25, 0.45, []string{"view"}},
		{0.30, 0.10, 0.60, nil},
	} {
		probs := map[string]float64{"view": tc.view, "grep": tc.grep, "bash": tc.bash}
		score, tools := pick(probs, []string{"view", "grep", "bash"})
		if !slices.Equal(tools, tc.want) || score != 1-tc.bash {
			t.Errorf("%v: score %.2f tools %v, want %.2f and %v", probs, score, tools, 1-tc.bash, tc.want)
		}
	}
}

func TestAFailedToolQuestionSuggestsNothing(t *testing.T) {
	var up atomic.Bool
	down := New(Conf{URL: server(t, commit, &up), Model: "m"}, func(string) {})
	if s := down.Suggest(context.Background(), "cat a.go", allTools); s.Known() {
		t.Errorf("a 503 produced a suggestion: %+v", s)
	}
	var hits atomic.Int32
	partial := New(Conf{URL: suggestServer(t, map[string]float64{"view": 0.9, "bash": 0.1}, &hits, nil), Model: "m"}, func(string) {})
	if s := partial.Suggest(context.Background(), "cat a.go", allTools); s.Known() || len(s.Tools) > 0 {
		t.Errorf("an answer missing grep and glob produced a suggestion: %+v", s)
	}
}

func TestASuggestionIsLoggedOnTheVerdictsLine(t *testing.T) {
	log := filepath.Join(t.TempDir(), "verdicts.jsonl")
	var hits atomic.Int32
	g := Gate{Client: New(Conf{URL: suggestServer(t, map[string]float64{"view": 0.8, "grep": 0.1, "bash": 0.1}, &hits, nil), Model: "m", Log: log}, nil),
		Criteria: coder}
	v := Verdict{Agent: "coder", Top: Network, Danger: 0.9, Threshold: 0.32, Flagged: true}
	s := g.Suggest(context.Background(), "cat a.go", []string{"view", "grep"})
	g.RecordSuggested("cat a.go", v, s, "suggested", "accepted")
	data, _ := os.ReadFile(log)
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("%d lines, want one", n)
	}
	e := logLine(t, log)
	if e["decision"] != "suggested" || e["suggestion_outcome"] != "accepted" || e["top"] != "network" ||
		e["suggestion_score"] == nil || e["suggestion_ms"] == nil || e["human"] != nil {
		t.Errorf("suggestion not logged with its verdict: %v", e)
	}
	if tools, _ := e["suggested_tools"].([]any); len(tools) != 1 || tools[0] != "view" {
		t.Errorf("suggested tools %v, want [view]", e["suggested_tools"])
	}
	if probs, _ := e["suggestion_probabilities"].(map[string]any); probs["view"] != 0.8 {
		t.Errorf("probabilities %v", e["suggestion_probabilities"])
	}
	id := SuggestionID([]string{"view", "grep"})
	if e["suggestion_questions"] != id || id == QuestionsID() || id == SuggestionID(allTools) {
		t.Errorf("suggestion question id %v, want its own %s", e["suggestion_questions"], id)
	}
	saved, err := os.ReadFile(filepath.Join(filepath.Dir(log), "questions-"+id+".json"))
	if err != nil {
		t.Fatalf("the tool question was not saved: %v", err)
	}
	keyOrder(t, saved, "view", "grep", "bash")
}

func TestAnUnattendedDeclineIsLoggedAsRefused(t *testing.T) {
	log := filepath.Join(t.TempDir(), "verdicts.jsonl")
	g := Gate{Client: New(Conf{Model: "m", Log: log}, nil), Criteria: coder, Unattended: func() bool { return true }}
	s := Suggestion{Menu: []string{"view", "bash"}, Probabilities: map[string]float64{"view": 0.9, "bash": 0.1}, Score: 0.9, Tools: []string{"view"}}
	g.RecordSuggested("cat a.go", Verdict{Agent: "coder", Top: Network, Flagged: true}, s, "declined", "declined→declined")
	if e := logLine(t, log); e["decision"] != "refused" || e["suggestion_outcome"] != "declined→refused" || e["human"] != nil {
		t.Errorf("unattended decline logged as a person's: %v", e)
	}
}

func TestARuleSuggestionIsLoggedWithItsToolsAndSource(t *testing.T) {
	log := filepath.Join(t.TempDir(), "verdicts.jsonl")
	g := Gate{Client: New(Conf{Model: "m", Log: log}, nil), Criteria: coder}
	g.RecordSuggested("sed -n 1,5p a.go", Verdict{}, Suggestion{Tools: []string{"view"}, Source: SourceRule}, "suggested", "accepted")
	e := logLine(t, log)
	if e["suggestion_source"] != "rule" || fmt.Sprint(e["suggested_tools"]) != "[view]" || e["suggestion_outcome"] != "accepted" {
		t.Errorf("rule suggestion logged as %v", e)
	}
	if e["suggestion_score"] != nil || e["danger"] != nil {
		t.Errorf("a rule suggestion carries no guardian scores, got %v", e)
	}
}

// Nobody answered, so a refused background command must not read as a person's judgement.
func TestAnUnattendedRefusalCarriesNoHumanLabel(t *testing.T) {
	log := filepath.Join(t.TempDir(), "verdicts.jsonl")
	g := Gate{Client: New(Conf{URL: server(t, commit, nil), Model: "m", Log: log}, nil), Criteria: coder,
		Unattended: func() bool { return true }}
	g.Record("jj commit -m x", g.Screen(context.Background(), "jj commit -m x"), "declined")
	if e := logLine(t, log); e["decision"] != "refused" || e["human"] != nil || e["misclassified"] != nil {
		t.Errorf("unattended refusal logged as a person's decision: %v", e)
	}
}

func TestConfigWithNoSettingsFileUsesTheDefaultServerAndModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := Config()
	if c.URL != "http://127.0.0.1:11434" || c.Model != "d3:2b" {
		t.Errorf("conf = %+v, want the default server and model", c)
	}
	if want := filepath.Join(home, ".local/share/alux/guardian/verdicts.jsonl"); c.Log != want {
		t.Errorf("log = %q, want %q", c.Log, want)
	}
}

func TestOllamaVersionsCompareNumericallyPerComponent(t *testing.T) {
	for _, tc := range []struct {
		v     string
		older bool
	}{
		{"0.40.8", true},
		{"0.9.12", true},
		{"0.40", true},
		{"0.40.9-rc1", false},
		{"0.40.9", false},
		{"0.40.10", false},
		{"0.41.0-rc1", false},
		{"1.0.0", false},
		{"", false},
		{"dev", false},
	} {
		if got := olderThan(tc.v, minOllama); got != tc.older {
			t.Errorf("olderThan(%q, %q) = %v, want %v", tc.v, minOllama, got, tc.older)
		}
	}
}

func TestCheckNamesTheUpgradeWhenOllamaIsTooOld(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			fmt.Fprint(w, `{"version": "0.39.2"}`)
			return
		}
		fmt.Fprint(w, `{"models": [{"name": "d3:2b"}]}`)
	}))
	defer srv.Close()
	err := Check(context.Background(), Conf{URL: srv.URL, Model: "d3:2b"})
	if err == nil || !strings.Contains(err.Error(), "brew upgrade ollama") {
		t.Errorf("an old server: err = %v", err)
	}
}

func TestCheckNamesWhatToDoWhenOllamaOrTheModelIsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"models": [{"name": "d3:2b"}, {"name": "other:latest"}]}`)
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := Check(ctx, Conf{URL: srv.URL, Model: "d3:2b"}); err != nil {
		t.Errorf("a pulled model: %v", err)
	}
	if err := Check(ctx, Conf{URL: srv.URL, Model: "other"}); err != nil {
		t.Errorf("a bare name matches its :latest tag: %v", err)
	}
	if err := Check(ctx, Conf{URL: srv.URL, Model: "d3:9b"}); err == nil || !strings.Contains(err.Error(), "`ollama pull d3:9b`") {
		t.Errorf("an unpulled model: err = %v", err)
	}
	srv.Close()
	if err := Check(ctx, Conf{URL: srv.URL, Model: "d3:2b"}); err == nil || !strings.Contains(err.Error(), "ollama.com") {
		t.Errorf("no server: err = %v", err)
	}
}

// A model that always favours whichever tier is listed first scores small and large evenly once both orders are asked.
func TestTierAveragesAwayAFirstOptionLean(t *testing.T) {
	var tasks []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var req struct {
			State map[string]string `json:"state"`
		}
		_ = json.Unmarshal(data, &req)
		tasks = append(tasks, req.State["task"])
		small, large := 0.15, 0.7
		if bytes.Index(data, []byte(`"small":`)) < bytes.Index(data, []byte(`"large":`)) {
			small, large = large, small
		}
		fmt.Fprintf(w, `{"answers": {"tier": {"type": "choice", "probabilities": {"small": %g, "medium": 0.15, "large": %g}}}}`, small, large)
	}))
	t.Cleanup(srv.Close)

	scores, ok := New(Conf{URL: srv.URL, Model: "m"}, nil).Tier(context.Background(), "add a flag")
	if !ok || scores[TierSmall] != scores[TierLarge] || scores[TierMedium] != 0.15 {
		t.Fatalf("scores = %v, %v; want small and large equal and medium 0.15", scores, ok)
	}
	if !slices.Equal(tasks, []string{"add a flag", "add a flag"}) {
		t.Errorf("tasks sent = %q, want the task twice", tasks)
	}
}
