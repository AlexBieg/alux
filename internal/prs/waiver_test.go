package prs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/taint"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

type handler func(*Poller, context.Context, agent.Call, agent.Callbacks) anthropic.ContentBlockParamUnion

func waivedCtx() context.Context { return agent.WithSkillWaiver(context.Background()) }

func counting(asks *int) agent.Callbacks {
	return agent.Callbacks{Require: func(context.Context, string, string, string) bool { *asks++; return true }}
}

type fakeJj struct {
	calls   [][]string
	dirs    []string
	remotes string
	change  string
}

func (f *fakeJj) run(_ context.Context, dir string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	f.dirs = append(f.dirs, dir)
	if slices.Equal(args, []string{"git", "remote", "list"}) {
		return f.remotes, nil
	}
	if slices.Contains(args, "log") {
		return f.change + "\n", nil
	}
	return "", nil
}

func (f *fakeJj) changes() [][]string {
	var out [][]string
	for _, c := range f.calls {
		if c[0] == "bookmark" || (c[0] == "git" && c[1] == "push") {
			out = append(out, c)
		}
	}
	return out
}

const (
	originOR = "origin https://github.com/o/r.git\n"
	repoInfo = `{"default_branch":"trunk"}`
)

const (
	draftPR  = `{"title":"t","draft":true,"state":"open"}`
	openPR   = `{"title":"t","draft":false,"state":"open"}`
	closedPR = `{"title":"t","draft":false,"state":"closed"}`
)

func TestAWriteAsksOnlyWhereItReachesReviewersOrTheMergeQueue(t *testing.T) {
	video := file(t, "demo.mp4", 1000)
	for _, tc := range []struct {
		name  string
		pr    string
		run   handler
		input string
		asks  int
	}{
		{"create without files", "", (*Poller).runCreate, `{"head":"h","title":"t","body":"b"}`, 0},
		{"create with a file", "", (*Poller).runCreate, `{"head":"h","title":"t","body":"![](` + video + `)","attachments":["` + video + `"]}`, 1},
		{"bugbot on a draft", draftPR, (*Poller).runReviewBot, `{"number":9}`, 0},
		{"bugbot on a ready PR", openPR, (*Poller).runReviewBot, `{"number":9}`, 1},
		{"labels on a draft", draftPR, (*Poller).runEdit, `{"number":9,"add_labels":["x"]}`, 0},
		{"labels on a ready PR", openPR, (*Poller).runEdit, `{"number":9,"add_labels":["x"]}`, 0},
		{"title on a draft", draftPR, (*Poller).runEdit, `{"number":9,"title":"n"}`, 0},
		{"body on a ready PR", openPR, (*Poller).runEdit, `{"number":9,"body":"n"}`, 0},
		{"ready on a draft", draftPR, (*Poller).runEdit, `{"number":9,"ready":true}`, 1},
		{"the ready label with ready", draftPR, (*Poller).runEdit, `{"number":9,"ready":true,"add_labels":["ready"]}`, 1},
		{"the merge label", openPR, (*Poller).runEdit, `{"number":9,"add_labels":["x","Merge-Queue"]}`, 1},
		{"title on a closed PR", closedPR, (*Poller).runEdit, `{"number":9,"title":"n"}`, 1},
	} {
		f := (&fakeGitHub{}).on("GET repos/o/r/pulls/9", tc.pr).on("POST repos/o/r/issues/9/comments", `{"html_url":"https://x/c"}`)
		p := authorPoller(f, &fakeGh{out: "https://github.com/o/r/pull/9\n"}, "h264")
		p.conf.Repos[0].ReadyLabel, p.conf.Repos[0].MergeLabel = "Ready", "merge-queue"
		asks := 0
		if res := tc.run(p, waivedCtx(), call(tc.input), counting(&asks)); isError(res) || asks != tc.asks {
			t.Errorf("%s: asked %d times, want %d (error %v)", tc.name, asks, tc.asks, isError(res))
		}
	}
}

func TestOutsideASkillRunDraftWritesRunUnasked(t *testing.T) {
	for name, tc := range map[string]struct {
		run   handler
		input string
		asks  int
	}{
		"create": {(*Poller).runCreate, `{"head":"h","title":"t","body":"b"}`, 0},
		"bugbot": {(*Poller).runReviewBot, `{"number":9}`, 0},
		"labels": {(*Poller).runEdit, `{"number":9,"add_labels":["x"]}`, 0},
		"push":   {(*Poller).runPush, `{"bookmark":"b","rev":"@-"}`, 0},
	} {
		f := (&fakeGitHub{}).on("GET repos/o/r/pulls/9", draftPR).on("pulls?head=", `[]`).on("POST repos/o/r/issues/9/comments", `{"html_url":"https://x/c"}`).on("GET repos/o/r", repoInfo)
		p := authorPoller(f, &fakeGh{}, "h264")
		p.jj = (&fakeJj{remotes: originOR}).run
		asks := 0
		tc.run(p, context.Background(), call(tc.input), counting(&asks))
		if asks != tc.asks {
			t.Errorf("%s: asked %d times, want %d", name, asks, tc.asks)
		}
	}
}

func TestAThreadReplyAsksEvenUnderTheWaiver(t *testing.T) {
	f := (&fakeGitHub{}).on("reviewThreads", twoThreads).
		on("POST repos/o/r/pulls/1/comments/9/replies", `{"id":77,"html_url":"https://x/77"}`)
	asks := 0
	poller(f).runThreadReply(waivedCtx(), call(`{"number":1,"comment_id":9,"body":"x"}`), counting(&asks))
	if asks != 1 {
		t.Errorf("asked %d times, want 1", asks)
	}
}

func TestAWaivedWriteAsksAgainOncePHIIsRead(t *testing.T) {
	taint.Set()
	defer taint.ResetForTest()
	for name, tc := range map[string]struct {
		run   handler
		input string
	}{
		"create": {(*Poller).runCreate, `{"head":"h","title":"t","body":"b"}`},
		"bugbot": {(*Poller).runReviewBot, `{"number":9}`},
		"labels": {(*Poller).runEdit, `{"number":9,"add_labels":["x"]}`},
		"push":   {(*Poller).runPush, `{"bookmark":"b","rev":"@-"}`},
	} {
		f := (&fakeGitHub{}).on("GET repos/o/r/pulls/9", draftPR).on("pulls?head=", `[]`).on("POST repos/o/r/issues/9/comments", `{"html_url":"https://x/c"}`).on("GET repos/o/r", repoInfo)
		p := authorPoller(f, &fakeGh{}, "h264")
		p.jj = (&fakeJj{remotes: originOR}).run
		asks := 0
		tc.run(p, waivedCtx(), call(tc.input), counting(&asks))
		if asks != 1 {
			t.Errorf("%s: asked %d times, want 1", name, asks)
		}
	}
}

func posts(f *fakeGitHub) []string {
	var out []string
	for _, r := range f.sent {
		if r.method == "POST" {
			out = append(out, r.path)
		}
	}
	return out
}

func TestTheReviewBotByHeadPostsOnThatBookmarksOpenPR(t *testing.T) {
	f := (&fakeGitHub{}).on("pulls?head=o:b&state=open", `[{"number":9}]`).on("GET repos/o/r/pulls/9", draftPR).
		on("POST repos/o/r/issues/9/comments", `{"html_url":"https://x/c"}`)
	asks := 0
	if res := poller(f).runReviewBot(waivedCtx(), call(`{"head":"b"}`), counting(&asks)); isError(res) || asks != 0 {
		t.Fatalf("by head: error %v, asks %d", isError(res), asks)
	}
	if p := posts(f); !slices.Equal(p, []string{"repos/o/r/issues/9/comments"}) {
		t.Errorf("posted %v", p)
	}
	for _, input := range []string{`{}`, `{"number":9,"head":"b"}`} {
		g := &fakeGitHub{}
		if res := poller(g).runReviewBot(waivedCtx(), call(input), counting(new(int))); !isError(res) || len(posts(g)) != 0 {
			t.Errorf("%s: not refused", input)
		}
	}
	none := (&fakeGitHub{}).on("pulls?head=o:b&state=open", `[]`)
	if res := poller(none).runReviewBot(waivedCtx(), call(`{"head":"b"}`), counting(new(int))); !isError(res) || len(posts(none)) != 0 {
		t.Error("a bookmark with no open PR was not refused")
	}
}

func TestAWaivedCreateLeavesTheNextStepToTheSkillRun(t *testing.T) {
	for _, waived := range []bool{true, false} {
		ctx := context.Background()
		if waived {
			ctx = waivedCtx()
		}
		asks := 0
		res := authorPoller(&fakeGitHub{}, &fakeGh{out: "https://github.com/o/r/pull/9\n"}, "h264").
			runCreate(ctx, call(`{"head":"h","title":"t","body":"b"}`), counting(&asks))
		text := res.OfToolResult.Content[0].OfText.Text
		if got := strings.Contains(text, "Run the review bot on it with pr_review_bot_run"); got == waived {
			t.Errorf("waived=%v: result %q", waived, text)
		}
	}
}

func TestPushToAPROpenForReviewAsksEvenUnderTheWaiver(t *testing.T) {
	for _, declined := range []bool{false, true} {
		jj := &fakeJj{remotes: originOR}
		p := poller((&fakeGitHub{}).on("pulls?head=o:b&state=all", `[{"number":3,"state":"open","draft":false,"merged_at":null,"html_url":"https://x/3"}]`).on("GET repos/o/r", repoInfo))
		p.jj = jj.run
		asks := 0
		cb := agent.Callbacks{Require: func(context.Context, string, string, string) bool { asks++; return !declined }}
		res := p.runPush(waivedCtx(), call(`{"bookmark":"b","rev":"@-"}`), cb)
		if asks != 1 || isError(res) != declined || (len(jj.changes()) == 2) == declined {
			t.Errorf("declined=%v: asks %d, error %v, jj %v", declined, asks, isError(res), jj.calls)
		}
	}
}

func TestPushTellsOnPushTheChangeTheBookmarkNowPointsAt(t *testing.T) {
	jj := &fakeJj{remotes: originOR, change: "znlxkrttkyopfull"}
	p := poller((&fakeGitHub{}).on("pulls?head=o:b&state=all", `[]`).on("GET repos/o/r", repoInfo))
	p.jj = jj.run
	var heard string
	p.OnPush = func(change string) string { heard = change; return "Run 13 shipped." }
	res := p.runPush(waivedCtx(), call(`{"bookmark":"b","rev":"@-"}`), counting(new(int)))
	text := res.OfToolResult.Content[0].OfText.Text
	if isError(res) || heard != "znlxkrttkyopfull" || !strings.Contains(text, "Run 13 shipped.") {
		t.Errorf("heard %q, result %q", heard, text)
	}
}

func TestPushRefusesAMergedPR(t *testing.T) {
	for name, list := range map[string]string{
		"merged": `[{"number":3,"state":"closed","draft":false,"merged_at":"2026-10-01T00:00:00Z","html_url":"https://x/3"}]`,
		"merged behind a draft": `[{"number":4,"state":"open","draft":true,"merged_at":null,"html_url":"https://x/4"},` +
			`{"number":3,"state":"closed","draft":false,"merged_at":"2026-10-01T00:00:00Z","html_url":"https://x/3"}]`,
	} {
		jj := &fakeJj{remotes: originOR}
		p := poller((&fakeGitHub{}).on("pulls?head=o:b&state=all", list).on("GET repos/o/r", repoInfo))
		p.jj = jj.run
		asks := 0
		res := p.runPush(waivedCtx(), call(`{"bookmark":"b","rev":"@-"}`), counting(&asks))
		if !isError(res) || len(jj.changes()) != 0 || asks != 0 {
			t.Errorf("%s: pushed or asked (jj %v, asks %d)", name, jj.calls, asks)
		}
	}
}

func TestPushRefusesABookmarkOrRevThatJjWouldReadAsAFlag(t *testing.T) {
	for _, input := range []string{`{"bookmark":"--allow-backwards","rev":"@-"}`, `{"bookmark":"b","rev":"--all"}`, `{"bookmark":" ","rev":"@-"}`, `{"bookmark":"b"}`} {
		jj := &fakeJj{}
		p := poller(&fakeGitHub{})
		p.jj = jj.run
		if res := p.runPush(waivedCtx(), call(input), counting(new(int))); !isError(res) || len(jj.calls) != 0 {
			t.Errorf("%s: not refused (jj %v)", input, jj.calls)
		}
	}
}

func TestPushMovesTheBookmarkThenPushesItInTheSessionDir(t *testing.T) {
	for name, list := range map[string]string{
		"no PR":    `[]`,
		"a draft":  `[{"number":4,"state":"open","draft":true,"merged_at":null,"html_url":"https://x/4"}]`,
		"a closed": `[{"number":2,"state":"closed","draft":false,"merged_at":null,"html_url":"https://x/2"}]`,
	} {
		jj := &fakeJj{remotes: originOR}
		p := poller((&fakeGitHub{}).on("pulls?head=o:b&state=all", list).on("GET repos/o/r", repoInfo))
		p.jj = jj.run
		asks := 0
		ctx := tools.WithDir(waivedCtx(), "/work/tree")
		res := p.runPush(ctx, call(`{"bookmark":"b","rev":"@-"}`), counting(&asks))
		want := [][]string{{"bookmark", "set", "b", "-r", "@-"}, {"git", "push", "--remote", "origin", "--bookmark", "b"}}
		if isError(res) || asks != 0 || !slices.EqualFunc(jj.changes(), want, slices.Equal[[]string]) {
			t.Errorf("%s: jj %v, asks %d, error %v", name, jj.calls, asks, isError(res))
		}
		if !slices.Equal(jj.dirs, []string{"/work/tree", "/work/tree", "/work/tree"}) {
			t.Errorf("%s: ran in %v", name, jj.dirs)
		}
	}
}

func TestPushGoesToTheRemoteThatPointsAtTheRepoAndRefusesWhenNoneDoes(t *testing.T) {
	for name, tc := range map[string]struct {
		remotes, want string
	}{
		"https":         {"origin https://github.com/o/r.git\n", "origin"},
		"scp-style ssh": {"fork git@github.com:me/r.git\nup git@github.com:o/r.git\n", "up"},
		"ssh url":       {"fork ssh://git@github.com/me/r\nup ssh://git@github.com/O/R.git\n", "up"},
		"another repo":  {"origin https://github.com/o/other.git\n", ""},
		"another host":  {"origin https://example.com/o/r.git\n", ""},
		"a longer name": {"origin https://github.com/o/r-fork.git\n", ""},
		"no remotes":    {"", ""},
		"a local path":  {"origin /work/o/r\n", ""},
	} {
		jj := &fakeJj{remotes: tc.remotes}
		p := poller((&fakeGitHub{}).on("pulls?head=o:b&state=all", `[]`).on("GET repos/o/r", repoInfo))
		p.jj = jj.run
		asks := 0
		res := p.runPush(waivedCtx(), call(`{"bookmark":"b","rev":"@-"}`), counting(&asks))
		changes := jj.changes()
		if tc.want == "" {
			if !isError(res) || len(changes) != 0 || asks != 0 {
				t.Errorf("%s: not refused (jj %v)", name, jj.calls)
			}
			continue
		}
		if isError(res) || len(changes) != 2 || !slices.Equal(changes[1], []string{"git", "push", "--remote", tc.want, "--bookmark", "b"}) {
			t.Errorf("%s: jj %v", name, jj.calls)
		}
	}
}

func TestPushRefusesTheDefaultBranchAndTrunkNames(t *testing.T) {
	for _, bookmark := range []string{"trunk", "main", "master"} {
		jj := &fakeJj{remotes: originOR}
		p := poller((&fakeGitHub{}).on("pulls?head=o:"+bookmark+"&state=all", `[]`).on("GET repos/o/r", repoInfo))
		p.jj = jj.run
		asks := 0
		res := p.runPush(waivedCtx(), call(`{"bookmark":"`+bookmark+`","rev":"@-"}`), counting(&asks))
		if !isError(res) || len(jj.changes()) != 0 || asks != 0 {
			t.Errorf("%s: not refused (jj %v, asks %d)", bookmark, jj.calls, asks)
		}
	}
}

func TestEveryWriteToolRefusesARepoThatIsNotConfigured(t *testing.T) {
	for name, tc := range map[string]struct {
		run   handler
		input string
	}{
		"create": {(*Poller).runCreate, `{"repo":"x/y","head":"h","title":"t","body":"b"}`},
		"edit":   {(*Poller).runEdit, `{"repo":"x/y","number":9,"add_labels":["x"]}`},
		"bugbot": {(*Poller).runReviewBot, `{"repo":"x/y","number":9}`},
		"push":   {(*Poller).runPush, `{"repo":"x/y","bookmark":"b","rev":"@-"}`},
	} {
		f := &fakeGitHub{}
		gh, jj := &fakeGh{}, &fakeJj{remotes: "origin https://github.com/x/y.git\n"}
		p := authorPoller(f, gh, "h264")
		p.jj = jj.run
		asks := 0
		res := tc.run(p, waivedCtx(), call(tc.input), counting(&asks))
		if !isError(res) || !strings.Contains(res.OfToolResult.Content[0].OfText.Text, "o/r") {
			t.Errorf("%s: not refused with the allowed repos named", name)
		}
		if len(gh.calls) != 0 || len(f.sent) != 0 || len(jj.calls) != 0 || asks != 0 {
			t.Errorf("%s: reached out (gh %v, github %v, jj %v, asks %d)", name, gh.calls, f.sent, jj.calls, asks)
		}
	}
}
