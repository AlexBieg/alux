package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mockapi"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

func sourceFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func viewTurn(id, path string, viewRange ...int) anthropic.MessageParam {
	input := map[string]any{"command": "view", "path": path}
	if viewRange != nil {
		input["view_range"] = viewRange
	}
	return anthropic.NewAssistantMessage(anthropic.NewToolUseBlock(id, input, editorName))
}

func viewResult(id string, failed bool) anthropic.MessageParam {
	return anthropic.NewUserMessage(anthropic.NewToolResultBlock(id, "seen", failed))
}

func seedPaths(files []seedFile) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.path)
	}
	return out
}

func TestOnlyFilesTheWholePlanNamesAreSeeded(t *testing.T) {
	root := t.TempDir()
	pair := sourceFile(t, root, "internal/subagent/pair.go")
	agentGo := sourceFile(t, root, "internal/agent/agent.go")
	subagentGo := sourceFile(t, root, "internal/subagent/subagent.go")
	gomod := sourceFile(t, root, "go.mod")
	otherAgent := sourceFile(t, root, "other/agent.go")

	for _, tc := range []struct {
		name   string
		viewed []string
		plan   string
		want   []string
	}{
		{"absolute path", []string{pair}, "Change " + pair + " to seed.", []string{pair}},
		{"path relative to the root", []string{pair}, "Change internal/subagent/pair.go to seed.", []string{pair}},
		{"unique bare name", []string{pair, agentGo}, "In `pair.go`, add a field.", []string{pair}},
		{"root-level bare name", []string{gomod, pair}, "Leave go.mod alone.", []string{gomod}},
		{"a bare name that two viewed files share", []string{agentGo, otherAgent}, "Edit agent.go.", nil},
		{"a name inside a longer name", []string{agentGo, subagentGo}, "Edit subagent.go.", []string{subagentGo}},
		{"a name that ends a longer path", []string{pair}, "Edit other/internal/subagent/pair.go.", nil},
		{"viewed but not named", []string{pair, agentGo}, "Edit " + pair + ".", []string{pair}},
		{"nothing named", []string{pair}, "Refactor the planner.", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var history []anthropic.MessageParam
			for i, path := range tc.viewed {
				history = append(history, viewTurn(fmt.Sprint("v", i), path), viewResult(fmt.Sprint("v", i), false))
			}
			if got := seedPaths(selectSeeds(history, tc.plan, root, root)); !slices.Equal(got, tc.want) {
				t.Errorf("seeded %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAFileTheModelNamesButNeverViewedIsNotSeeded(t *testing.T) {
	root := t.TempDir()
	viewed := sourceFile(t, root, "a.go")
	unviewed := sourceFile(t, root, "b.go")
	history := []anthropic.MessageParam{viewTurn("v1", viewed), viewResult("v1", false)}

	got := seedPaths(selectSeeds(history, "Change "+viewed+" and "+unviewed+".", root, root))
	if !slices.Equal(got, []string{viewed}) {
		t.Errorf("seeded %v, want only %v", got, viewed)
	}
}

func TestDirectoriesMissingFilesAndFailedViewsAreNotSeeded(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	sourceFile(t, root, "pkg/x.go")
	missing := filepath.Join(root, "gone.go")
	failedView := sourceFile(t, root, "failed.go")
	history := []anthropic.MessageParam{
		viewTurn("v1", dir), viewResult("v1", false),
		viewTurn("v2", missing), viewResult("v2", false),
		viewTurn("v3", failedView, 500, 600), viewResult("v3", true),
	}

	if got := selectSeeds(history, dir+" "+missing+" "+failedView, root, root); len(got) != 0 {
		t.Errorf("seeded %v", seedPaths(got))
	}
}

func TestSeedsComeInTheOrderTheyWereFirstViewed(t *testing.T) {
	root := t.TempDir()
	a, b, c := sourceFile(t, root, "a.go"), sourceFile(t, root, "b.go"), sourceFile(t, root, "c.go")
	history := []anthropic.MessageParam{
		viewTurn("v1", c), viewTurn("v2", a), viewTurn("v3", b), viewTurn("v4", c, 1, 5),
	}

	if got := seedPaths(selectSeeds(history, a+" "+b+" "+c, root, root)); !slices.Equal(got, []string{c, a, b}) {
		t.Errorf("seeded %v, want %v", got, []string{c, a, b})
	}
}

func TestViewedRangesMergeAndAWholeViewAbsorbsThem(t *testing.T) {
	root := t.TempDir()
	path := sourceFile(t, root, "a.go")

	for _, tc := range []struct {
		name  string
		views [][]int
		want  []viewSpan
	}{
		{"adjacent", [][]int{{1, 10}, {11, 20}}, []viewSpan{{1, 20}}},
		{"overlapping", [][]int{{1, 10}, {5, 30}}, []viewSpan{{1, 30}}},
		{"contained", [][]int{{1, 30}, {5, 10}}, []viewSpan{{1, 30}}},
		{"apart", [][]int{{40, 50}, {1, 10}}, []viewSpan{{1, 10}, {40, 50}}},
		{"to the end absorbs what follows", [][]int{{50, -1}, {60, 70}}, []viewSpan{{50, -1}}},
		{"to the end after a range", [][]int{{20, 30}, {25, -1}}, []viewSpan{{20, -1}}},
		{"ranges that join into the whole file", [][]int{{1, 10}, {8, -1}}, nil},
		{"a whole view first", [][]int{nil, {5, 10}}, nil},
		{"a whole view last", [][]int{{5, 10}, {20, 30}, nil}, nil},
		{"a view from the first line to the end", [][]int{{5, 10}, {1, -1}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var history []anthropic.MessageParam
			for i, r := range tc.views {
				history = append(history, viewTurn(fmt.Sprint("v", i), path, r...), viewResult(fmt.Sprint("v", i), false))
			}
			got := selectSeeds(history, path, root, root)
			if len(got) != 1 || !slices.Equal(got[0].spans, tc.want) {
				t.Errorf("got %+v, want spans %v", got, tc.want)
			}
		})
	}
}

type wireBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Text      string          `json:"text"`
	Content   []struct {
		Text string `json:"text"`
	} `json:"content"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

func messagesOf(t *testing.T, raw string) []wireMessage {
	t.Helper()
	var req struct {
		Messages []wireMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	return req.Messages
}

func withViewingPlanner(def *Definition) *Definition {
	withPlanner(def)
	planner := def.Peer.Planner
	offered := planner.Tools
	planner.Tools = func() []anthropic.ToolUnionParam {
		text := map[string]any{"type": "string"}
		return append(offered(), strict(editorName, "view", map[string]any{"command": text, "path": text}, []string{"command"}))
	}
	planner.Handlers[editorName] = func(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
		return anthropic.NewToolResultBlock(call.ID, "PLANNING-VIEW-RESULT", false)
	}
	def.Peer.Seeded = func() string { return drivingSystem + "\nSEEDED-SYSTEM" }
	return def
}

func viewUse(id, path string, viewRange ...int) mockapi.ToolUse {
	input := map[string]any{"command": "view", "path": path}
	if viewRange != nil {
		input["view_range"] = viewRange
	}
	return mockapi.ToolUse{ID: id, Name: editorName, Input: input}
}

func finishing(turns ...mockapi.Turn) []mockapi.Turn {
	return append(turns, mockapi.Turn{Text: "Done."}, mockapi.Turn{Text: none}, mockapi.Turn{Text: "Nothing outstanding."})
}

func TestTheDriverStartsWithFreshReadsOfWhatThePlanNames(t *testing.T) {
	dir := t.TempDir()
	a, b, c, d := sourceFile(t, dir, "a.go"), sourceFile(t, dir, "b.go"), sourceFile(t, dir, "c.go"), sourceFile(t, dir, "d.go")
	plan := "PLAN-SENTINEL: change " + a + ", " + b + " and " + d + "."

	m, server := manager(t, withViewingPlanner(paired()), finishing(
		mockapi.Turn{Tools: []mockapi.ToolUse{viewUse("p1", a), viewUse("p2", b, 2, 3), viewUse("p3", c)}},
		mockapi.Turn{Text: plan},
	)...)
	dispatch(t, m)

	driven := requestsTo(t, server, drivingSystem)
	if len(driven) != 1 {
		t.Fatalf("the driver made %d requests, want 1", len(driven))
	}
	msgs := messagesOf(t, driven[0])
	if len(msgs) != 5 {
		t.Fatalf("the driver's first request holds %d messages, want the plan and two reads", len(msgs))
	}
	for i, msg := range msgs {
		if want := []string{"user", "assistant"}[i%2]; msg.Role != want {
			t.Errorf("message %d is from %s, want %s", i, msg.Role, want)
		}
	}
	if len(msgs[0].Content) != 1 || !strings.Contains(msgs[0].Content[0].Text, "PLAN-SENTINEL") {
		t.Errorf("the first message is not the task and plan: %+v", msgs[0])
	}

	var ids, read []string
	var ranges [][]int
	for i := 1; i < len(msgs); i += 2 {
		use, result := msgs[i].Content, msgs[i+1].Content
		if len(use) != 1 || use[0].Type != "tool_use" || use[0].Name != editorName {
			t.Fatalf("message %d is not a single editor call: %+v", i, use)
		}
		if len(result) != 1 || result[0].Type != "tool_result" || result[0].ToolUseID != use[0].ID {
			t.Fatalf("message %d does not answer the call before it: %+v", i+1, result)
		}
		if len(result[0].Content) != 1 || result[0].Content[0].Text != "READ-RESULT-BODY" {
			t.Errorf("message %d is not the driver's own read: %+v", i+1, result[0].Content)
		}
		var in struct {
			Command   string `json:"command"`
			Path      string `json:"path"`
			ViewRange []int  `json:"view_range"`
		}
		if err := json.Unmarshal(use[0].Input, &in); err != nil || in.Command != "view" {
			t.Fatalf("message %d is not a view: %s", i, use[0].Input)
		}
		ids, read, ranges = append(ids, use[0].ID), append(read, in.Path), append(ranges, in.ViewRange)
	}
	if !slices.Equal(read, []string{a, b}) {
		t.Errorf("the driver was given %v, want %v: what the plan names and the planner viewed", read, []string{a, b})
	}
	if ranges[0] != nil || !slices.Equal(ranges[1], []int{2, 3}) {
		t.Errorf("view ranges = %v, want the whole of a.go and lines 2-3 of b.go", ranges)
	}
	if ids[0] == ids[1] {
		t.Errorf("both reads carry the id %s", ids[0])
	}
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if strings.Contains(block.Type, "thinking") {
				t.Errorf("a %s block is in the driver's history", block.Type)
			}
		}
	}
	for _, leaked := range []string{"PLANNING-VIEW-RESULT", c} {
		if strings.Contains(driven[0], leaked) {
			t.Errorf("%q reached the driver's first request", leaked)
		}
	}
	if n := strings.Count(driven[0], "PLAN-SENTINEL"); n != 1 {
		t.Errorf("the plan is in the driver's first request %d times, want once, as the task", n)
	}
	if !strings.Contains(driven[0], "SEEDED-SYSTEM") {
		t.Error("a seeded driver kept the prompt that tells it to read first")
	}
	if trail := rowFor(t, m, "driver").TrailText(); !strings.Contains(trail, "seeded 2 reads from the plan") {
		t.Errorf("the driver's row does not say it was seeded:\n%s", trail)
	}
}

func TestADriverWithNothingToSeedStartsAsItDoesToday(t *testing.T) {
	dir := t.TempDir()
	viewed := sourceFile(t, dir, "a.go")

	m, server := manager(t, withViewingPlanner(paired()), finishing(
		mockapi.Turn{Tools: []mockapi.ToolUse{viewUse("p1", viewed)}},
		mockapi.Turn{Text: "PLAN-SENTINEL: rework the loop."},
	)...)
	dispatch(t, m)

	driven := requestsTo(t, server, drivingSystem)
	if msgs := messagesOf(t, driven[0]); len(msgs) != 1 {
		t.Errorf("the driver's first request holds %d messages, want only the task and plan", len(msgs))
	}
	if strings.Contains(driven[0], "SEEDED-SYSTEM") {
		t.Error("an unseeded driver was given the seeded prompt")
	}
	if trail := rowFor(t, m, "driver").TrailText(); strings.Contains(trail, "seeded") {
		t.Errorf("an unseeded driver's row claims a seed:\n%s", trail)
	}
}

func TestSeededReadsStayUnderTheByteCapAndASkippedReadDoesNotStopTheRest(t *testing.T) {
	dir := t.TempDir()
	names := []string{"big.go", "gone.go", "fill.go", "over.go", "tiny.go"}
	var views []mockapi.ToolUse
	var plan []string
	for _, name := range names {
		path := sourceFile(t, dir, name)
		views = append(views, viewUse(name, path))
		plan = append(plan, path)
	}

	def := withViewingPlanner(paired())
	def.Handlers[editorName] = func(_ context.Context, call agent.Call, _ agent.Callbacks) anthropic.ContentBlockParamUnion {
		var in struct{ Path string }
		_ = json.Unmarshal(call.Input, &in)
		switch filepath.Base(in.Path) {
		case "big.go":
			return anthropic.NewToolResultBlock(call.ID, strings.Repeat("x", seedBytes+1), false)
		case "gone.go":
			return anthropic.NewToolResultBlock(call.ID, "no such file", true)
		case "fill.go":
			return anthropic.NewToolResultBlock(call.ID, strings.Repeat("f", seedBytes-5), false)
		case "over.go":
			return anthropic.NewToolResultBlock(call.ID, strings.Repeat("o", 10), false)
		}
		return anthropic.NewToolResultBlock(call.ID, "tiny!", false)
	}
	m, server := manager(t, def, finishing(
		mockapi.Turn{Tools: views},
		mockapi.Turn{Text: "PLAN-SENTINEL: " + strings.Join(plan, " ")},
	)...)
	dispatch(t, m)

	var kept []string
	msgs := messagesOf(t, requestsTo(t, server, drivingSystem)[0])
	for i := 1; i < len(msgs); i += 2 {
		var in struct{ Path string }
		_ = json.Unmarshal(msgs[i].Content[0].Input, &in)
		kept = append(kept, filepath.Base(in.Path))
	}
	if !slices.Equal(kept, []string{"fill.go", "tiny.go"}) {
		t.Errorf("seeded %v, want fill.go and tiny.go: oversized and failed reads dropped, the rest kept in order", kept)
	}
}

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seeding(agentName string, seeds ...map[string]any) map[string]any {
	return map[string]any{"agent": agentName, "name": "seeded", "objective": "fix it", "done": "fixed", "report": "what changed",
		"background": false, "seed": seeds}
}

func seededReads(t *testing.T, msgs []wireMessage) (paths, results []string) {
	t.Helper()
	for i := 1; i+1 < len(msgs); i += 2 {
		use, result := msgs[i].Content, msgs[i+1].Content
		if len(use) != 1 || use[0].Type != "tool_use" || len(result) != 1 || result[0].ToolUseID != use[0].ID {
			t.Fatalf("messages %d and %d are not a seeded read: %+v %+v", i, i+1, use, result)
		}
		var in struct{ Path string }
		_ = json.Unmarshal(use[0].Input, &in)
		var text []string
		for _, c := range result[0].Content {
			text = append(text, c.Text)
		}
		paths, results = append(paths, in.Path), append(results, strings.Join(text, "\n"))
	}
	return paths, results
}

func seedEvents(t *testing.T, m *Manager, dispatch string) []string {
	t.Helper()
	events, err := ReadTranscript(m.Transcripts, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Kind == TranscriptSeed {
			out = append(out, string(e.Input))
		}
	}
	return out
}

func TestASeededCoderStartsWithItsOwnReadsInItsWorkspace(t *testing.T) {
	m, server, layers := layeredCoder(t, mockapi.Turn{Text: "Done."}, mockapi.Turn{Text: "Done."})
	m.Transcripts = t.TempDir()
	put(t, filepath.Join(layers.origin, "a.go"), "MAIN-A\n")
	put(t, filepath.Join(layers.dir, "a.go"), "LAYER-A\n")
	put(t, filepath.Join(layers.dir, "b.go"), "LAYER-B\nSECOND\n")
	yes := agent.Callbacks{Approve: func(context.Context, string, string) bool { return true }}

	seeded := call(t, DispatchTool, seeding("coder",
		map[string]any{"path": filepath.Join(layers.origin, "a.go")},
		map[string]any{"path": "b.go", "view_range": []int{1, 1}}))
	if text, failed := toolText(t, m.runDispatch(context.Background(), seeded, yes)); failed {
		t.Fatal(text)
	}
	if text, failed := toolText(t, m.runDispatch(context.Background(), call(t, DispatchTool, seeding("coder")), yes)); failed {
		t.Fatal(text)
	}

	coded := requestsTo(t, server, "You are a coding subagent. Another agent, working in the user's terminal, has handed you a change it does not want to spend its own context making. You never talk to the user.")
	if len(coded) != 2 {
		t.Fatalf("the coder made %d requests, want one per dispatch", len(coded))
	}
	paths, results := seededReads(t, messagesOf(t, coded[0]))
	wantPaths := []string{filepath.Join(layers.dir, "a.go"), filepath.Join(layers.dir, "b.go")}
	if !slices.Equal(paths, wantPaths) {
		t.Fatalf("seeded %v, want %v", paths, wantPaths)
	}
	if !strings.Contains(results[0], "LAYER-A") || !strings.Contains(results[1], "LAYER-B") || strings.Contains(coded[0], "MAIN-A") ||
		strings.Contains(results[1], "SECOND") {
		t.Errorf("the seeded reads are not the workspace's own: %q", results)
	}
	const readFirst = "Read before you write."
	if strings.Contains(coded[0], readFirst) {
		t.Errorf("a seeded coder is still told %q", readFirst)
	}
	if !strings.Contains(coded[1], readFirst) || len(messagesOf(t, coded[1])) != 1 {
		t.Errorf("an unseeded coder lost %q or was seeded", readFirst)
	}

	runs := m.Roster.Runs()
	want := fmt.Sprintf(`[{"path":%q},{"path":%q,"view_range":[1,1]}]`, wantPaths[0], wantPaths[1])
	if got := seedEvents(t, m, runs[0].Dispatch); !slices.Equal(got, []string{want}) {
		t.Errorf("seed events %v, want %v", got, want)
	}
	if got := seedEvents(t, m, runs[1].Dispatch); len(got) != 0 {
		t.Errorf("an unseeded run recorded seeds %v", got)
	}
}

func TestADispatchSeedThePlanDoesNotNameReachesThePlannerAndTheDriver(t *testing.T) {
	seed := sourceFile(t, t.TempDir(), "a.go")
	def := withViewingPlanner(paired())
	def.Seedable = true
	m, server := manager(t, def, finishing(mockapi.Turn{Text: "PLAN-SENTINEL: rework the loop."})...)

	if text, failed := toolText(t, m.runDispatch(context.Background(),
		call(t, DispatchTool, seeding("driver", map[string]any{"path": seed})), agent.Callbacks{})); failed {
		t.Fatal(text)
	}

	planned, planResults := seededReads(t, messagesOf(t, requestsTo(t, server, planningSystem)[0]))
	if !slices.Equal(planned, []string{seed}) || planResults[0] != "PLANNING-VIEW-RESULT" {
		t.Errorf("the planner was seeded with %v %q, want its own read of %s", planned, planResults, seed)
	}
	driven, driveResults := seededReads(t, messagesOf(t, requestsTo(t, server, drivingSystem)[0]))
	if !slices.Equal(driven, []string{seed}) || driveResults[0] != "READ-RESULT-BODY" {
		t.Errorf("the driver was seeded with %v %q, want its own read of %s", driven, driveResults, seed)
	}
	if trail := rowFor(t, m, "driver").TrailText(); !strings.Contains(trail, "seeded 1 read from the dispatch and the plan") {
		t.Errorf("the driver's row does not say it was seeded:\n%s", trail)
	}
}

func TestAPlannersRelativeViewIsSeededFromTheRunsDirectory(t *testing.T) {
	dir := t.TempDir()
	want := sourceFile(t, dir, "a.go")
	m, server := manager(t, withViewingPlanner(paired()), finishing(
		mockapi.Turn{Tools: []mockapi.ToolUse{viewUse("p1", "a.go")}},
		mockapi.Turn{Text: "PLAN-SENTINEL: change a.go."},
	)...)

	if _, err := m.Dispatch(tools.WithDir(context.Background(), dir), "driver", "fix it", "", agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := seededReads(t, messagesOf(t, requestsTo(t, server, drivingSystem)[0])); !slices.Equal(got, []string{want}) {
		t.Errorf("seeded %v, want %v", got, []string{want})
	}
}

func TestAnAgentThatTakesNoSeedsIgnoresThemAndSaysSo(t *testing.T) {
	seed := sourceFile(t, t.TempDir(), "a.go")
	def := paired()
	def.Peer = nil
	m, server := manager(t, def, mockapi.Turn{Text: "Done."})
	m.Transcripts = t.TempDir()
	var logged []Dispatch
	m.Log = func(rec Dispatch) { logged = append(logged, rec) }

	text, failed := toolText(t, m.runDispatch(context.Background(),
		call(t, DispatchTool, seeding("driver", map[string]any{"path": seed})), agent.Callbacks{}))
	if len(logged) != 1 || !reflect.DeepEqual(logged[0].SeedsAsked, []Seed{{Path: seed}}) || len(logged[0].Seeds) != 0 {
		t.Errorf("logged %+v, want the asked-for seed and none read", logged)
	}
	if failed || !strings.Contains(text, "The seeds were ignored") {
		t.Errorf("the result does not say the seeds were ignored: %q", text)
	}
	if n := len(messagesOf(t, string(server.Requests[0]))); n != 1 {
		t.Errorf("the first request holds %d messages, want only the task", n)
	}
	if got := seedEvents(t, m, m.Roster.Runs()[0].Dispatch); len(got) != 0 {
		t.Errorf("an unseeded run recorded seeds %v", got)
	}
}
