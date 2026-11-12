package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// stuck is an agent whose one tool blocks until its run is cancelled.
func stuck(started chan<- struct{}) *Definition {
	def := echo(true)
	def.Tools = func() []anthropic.ToolUnionParam {
		return []anthropic.ToolUnionParam{strict("wait", "wait for something", map[string]any{}, nil)}
	}
	def.Handlers = map[string]agent.Handler{
		"wait": func(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
			cb.OnToolResult(call.ID, "waiting on the build", "", agent.KindVault)
			started <- struct{}{}
			<-ctx.Done()
			return anthropic.NewToolResultBlock(call.ID, "interrupted", true)
		},
	}
	return def
}

var waitOnce = mockapi.Turn{Tools: []mockapi.ToolUse{{ID: "t1", Name: "wait", Input: map[string]any{}}}}

func call(t *testing.T, name string, input map[string]any) agent.Call {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return agent.Call{ID: "c1", Name: name, Input: raw}
}

func toolText(t *testing.T, block anthropic.ContentBlockParamUnion) (string, bool) {
	t.Helper()
	res := block.OfToolResult
	if res == nil || len(res.Content) == 0 || res.Content[0].OfText == nil {
		t.Fatalf("not a text tool result: %+v", block)
	}
	return res.Content[0].OfText.Text, res.IsError.Value
}

// A stopped run still reports what it finished and what it left half-done.
func TestAStoppedRunReportsWhatItLeftUnfinished(t *testing.T) {
	started := make(chan struct{}, 1)
	m, _ := manager(t, stuck(started), waitOnce,
		mockapi.Turn{Text: "Half of view.go is rewritten. Still unfinished: the tests."})

	if _, err := m.Dispatch(context.Background(), "echo", "rewrite view.go", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	<-started
	id := m.Roster.Runs()[0].ID

	status, failed := toolText(t, m.runStatus(context.Background(), call(t, StatusTool, map[string]any{}), agent.Callbacks{}))
	if failed || !strings.Contains(status, "step 1/5") || !strings.Contains(status, "waiting on the build") {
		t.Errorf("status = %q, want the live run's step and activity", status)
	}

	if text, failed := toolText(t, m.runCancel(context.Background(), call(t, CancelTool, map[string]any{"run": id}), agent.Callbacks{})); failed {
		t.Fatalf("cancel refused: %s", text)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the stopped run never ended")
	}
	outcomes := m.Roster.TakeOutcomes()
	if len(outcomes) != 1 || outcomes[0].Err != "" {
		t.Fatalf("outcomes = %+v, want one report and no failure", outcomes)
	}
	if report := outcomes[0].Report; !strings.Contains(report, "Stopped at your request") ||
		!strings.Contains(report, "Still unfinished: the tests.") {
		t.Errorf("report = %q, want the stop named and the closing account", report)
	}
	if _, failed := toolText(t, m.runCancel(context.Background(), call(t, CancelTool, map[string]any{"run": id}), agent.Callbacks{})); !failed {
		t.Error("stopping a run that has already ended was not refused")
	}
}

// A stopped run whose closing report never arrives still ends.
func TestAStoppedRunWhoseClosingReportHangsStillEnds(t *testing.T) {
	was := closingWithin
	closingWithin = 50 * time.Millisecond
	t.Cleanup(func() { closingWithin = was })

	upstream := mockapi.New(waitOnce)
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var requests atomic.Int32
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			proxy.ServeHTTP(w, r)
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(release) })

	started := make(chan struct{}, 1)
	m, _ := manager(t, stuck(started))
	client := anthropic.NewClient(option.WithBaseURL(hang.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	m.Client = &client

	if _, err := m.Dispatch(context.Background(), "echo", "rewrite view.go", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := m.Roster.Stop(m.Roster.Runs()[0].ID); err != nil {
		t.Fatal(err)
	}
	if !m.Roster.Drain(5 * time.Second) {
		t.Fatal("the run waited on its closing report past the timeout")
	}
	outcomes := m.Roster.TakeOutcomes()
	if len(outcomes) != 1 || !strings.Contains(outcomes[0].Report, "closing report did not arrive") {
		t.Errorf("outcomes = %+v, want a report saying the closing account never came", outcomes)
	}
}

// Only a stop earns a closing report; an interrupted turn spends no request on one.
func TestAnInterruptedRunWritesNoClosingReport(t *testing.T) {
	started := make(chan struct{}, 1)
	def := stuck(started)
	def.Background = false
	m, server := manager(t, def, waitOnce, mockapi.Turn{Text: "should not be asked for"})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := m.Dispatch(ctx, "echo", "rewrite view.go", "", agent.Callbacks{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if got := server.RequestCount(); got != 1 {
		t.Errorf("requests = %d, want 1: an interrupted run was finalized", got)
	}
}
