package compact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func texts(ev []Event) string {
	var out []string
	for _, e := range ev {
		out = append(out, e.Text)
	}
	return strings.Join(out, "|")
}

func TestASecondCompactionCoversTheRawHistoryAndAClearDropsIt(t *testing.T) {
	first := []string{
		`{"kind":"user","text":"A"}`, `{"kind":"text","text":"a"}`,
		`{"kind":"reset"}`, `{"kind":"user","text":"Compacted history, steps 0-1."}`, `{"kind":"text","text":"b"}`,
	}
	ev, err := Load(writeLog(t, first...))
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(ev); got != "A|a|b" {
		t.Errorf("after a compaction Load = %q, want the raw history without the view", got)
	}
	ev, _ = Load(writeLog(t, append(first, `{"kind":"reset"}`, `{"kind":"user","text":"C"}`)...))
	if got := texts(ev); got != "C" {
		t.Errorf("after a /clear Load = %q, want only what followed it", got)
	}
}

func TestRecallPastTheCapNamesTheStepToContinueFrom(t *testing.T) {
	big := strings.Repeat("x", recallCap/2)
	ev := []Event{{Kind: "text", Text: big}, {Kind: "text", Text: big}, {Kind: "text", Text: "tail"}}
	out, err := Recall(ev, "0-2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "@1 ") || !strings.Contains(out, "recall 1-2 for the rest") {
		t.Errorf("a recall over the cap did not stop and name where to resume:\n%.200s", out[len(out)-200:])
	}
}

func TestWithoutEmbeddingTheViewIsStillBuiltAndOlderRepliesStayWhole(t *testing.T) {
	t.Setenv("ALUX_RECORDS_EMBED", "off")
	reply := "Intro.\nThe cache lives in compact.go. It is keyed by sentence.\nDone."
	ev := []Event{
		{Kind: "user", Text: "one"}, {Kind: "text", Text: reply},
		{Kind: "user", Text: "two"}, {Kind: "text", Text: "ok"},
		{Kind: "user", Text: "three"}, {Kind: "text", Text: "ok"},
	}
	view, err := Build(ev, "", filepath.Join(t.TempDir(), "v.bin"))
	if err == nil {
		t.Error("embedding was off but Build reported no error")
	}
	if !strings.HasPrefix(view, Marker) || !strings.Contains(view, reply) {
		t.Errorf("fallback view lost the older reply:\n%s", view)
	}
}

func TestAnUnknownToolKeepsCreatedAndReusedIdsAndTheNewestResultsStayWhole(t *testing.T) {
	t.Setenv("ALUX_RECORDS_EMBED", "off")
	old := strings.Repeat("old stage steps ", tailBudget/16)
	ev := []Event{
		{Kind: "user", Text: "go"},
		{Kind: "tool", Name: "made_up_tool", Output: "Stored rec_0123456789abcdef.\nSimilar: rec_0000000000000001"},
		{Kind: "tool", Name: "made_up_tool", Output: "Stored rec_1111111111111111."},
		{Kind: "tool", Name: "made_up_tool", Output: old},
		{Kind: "tool", Name: "made_up_tool", Input: []byte(`{"id":"rec_0123456789abcdef"}`), Output: "linked"},
		{Kind: "tool", Name: "made_up_tool", Output: "Stage base is open: run jj new main@origin"},
	}
	view, _ := BuildKeeping(ev, "", filepath.Join(t.TempDir(), "v.bin"))
	for _, want := range []string{"rec_0123456789abcdef @1", "rec_1111111111111111 @2", "run jj new main@origin"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lost %q:\n%s", want, view)
		}
	}
	if n := strings.Count(view, "rec_0123456789abcdef"); n != 1 {
		t.Errorf("an id both created and reused is listed %d times, want once:\n%s", n, view)
	}
	if strings.Contains(view, "rec_0000000000000001") || strings.Contains(view, old) {
		t.Errorf("view kept an unused id or an old result past the tail budget:\n%s", view)
	}
}

func TestTheLastTurnIsStillRunningOnlyWhenTheCutFallsBeforeItsReply(t *testing.T) {
	t.Setenv("ALUX_RECORDS_EMBED", "off")
	turn := []Event{
		{Kind: "user", Text: "fix it"},
		{Kind: "tool", Name: "bash", Input: []byte(`{"command":"go test ./..."}`), Output: "ok"},
	}
	for _, c := range []struct {
		name    string
		ev      []Event
		running bool
	}{
		{"cut after a tool result", turn, true},
		{"cut after the final reply", append(append([]Event{}, turn...), Event{Kind: "text", Text: "Done."}), false},
	} {
		view, _ := Build(c.ev, "", filepath.Join(t.TempDir(), "v.bin"))
		if got := strings.Contains(view, "still running at the cut"); got != c.running {
			t.Errorf("%s: still running = %v, want %v:\n%s", c.name, got, c.running, view)
		}
	}
}
