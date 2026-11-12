package prs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/agent"
)

type fakeGh struct {
	calls [][]string
	body  string
	out   string
}

func (f *fakeGh) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if i := slices.Index(args, "--body-file"); i >= 0 {
		b, _ := os.ReadFile(args[i+1])
		f.body = string(b)
	}
	return f.out, nil
}

func authorPoller(f *fakeGitHub, gh *fakeGh, codec string) *Poller {
	p := poller(f)
	p.gh = gh.run
	p.probe = func(context.Context, string) (string, float64, error) { return codec, 12, nil }
	return p
}

func file(t *testing.T, name string, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func approving(yes bool, shown *string) agent.Callbacks {
	return agent.Callbacks{Require: func(_ context.Context, _, preview, _ string) bool { *shown = preview; return yes }}
}

func TestCreateOpensADraftWithTheBodyAndUploadsOnlyOnYes(t *testing.T) {
	video := file(t, "demo.mp4", 1000)
	input := `{"head":"abc-1","title":"Add step","body":"Before and after:\n\n![](` + video + `)","attachments":["` + video + `"]}`
	for _, yes := range []bool{false, true} {
		gh := &fakeGh{out: "https://github.com/o/r/pull/9\n"}
		var shown string
		res := authorPoller(&fakeGitHub{}, gh, "h264").runCreate(context.Background(), call(input), approving(yes, &shown))
		if !strings.Contains(shown, "4 words") || !strings.Contains(shown, video) || !strings.Contains(shown, "H.264") {
			t.Errorf("the approval did not show the word count and the upload: %q", shown)
		}
		if ran := len(gh.calls) > 0; ran != yes {
			t.Fatalf("approved=%v but gh ran=%v", yes, ran)
		}
		if !yes {
			continue
		}
		args := gh.calls[0]
		if !slices.Contains(args, "--draft") || args[slices.Index(args, "--attach")+1] != video || args[slices.Index(args, "--repo")+1] != "o/r" {
			t.Errorf("gh args = %v", args)
		}
		if !strings.Contains(gh.body, "![]("+video+")") {
			t.Errorf("body sent = %q", gh.body)
		}
		if isError(res) {
			t.Errorf("result was an error")
		}
	}
}

func TestTheReadyLabelIsRefusedOnADraftUnlessItIsMarkedReady(t *testing.T) {
	draft := `{"title":"Add step","draft":true,"state":"open"}`
	withReady := func(p *Poller, label string) *Poller {
		p.conf.Repos[0].ReadyLabel = label
		return p
	}

	gh := &fakeGh{}
	var shown string
	f := (&fakeGitHub{}).on("GET repos/o/r/pulls/9", draft)
	res := withReady(authorPoller(f, gh, "h264"), "Ready").runEdit(context.Background(), call(`{"number":9,"add_labels":["ready"]}`), approving(true, &shown))
	if !isError(res) || len(gh.calls) != 0 || shown != "" {
		t.Fatalf("the ready label went on a draft: calls %v", gh.calls)
	}

	gh = &fakeGh{}
	f = (&fakeGitHub{}).on("GET repos/o/r/pulls/9", draft)
	res = withReady(authorPoller(f, gh, "h264"), "").runEdit(context.Background(), call(`{"number":9,"add_labels":["Ready"]}`), approving(true, &shown))
	if isError(res) || len(gh.calls) != 1 {
		t.Fatalf("with no ready label configured, a label was refused on a draft: calls %v", gh.calls)
	}

	gh = &fakeGh{}
	f = (&fakeGitHub{}).on("GET repos/o/r/pulls/9", draft)
	res = withReady(authorPoller(f, gh, "h264"), "Ready").runEdit(context.Background(), call(`{"number":9,"ready":true,"add_labels":["Ready"]}`), approving(true, &shown))
	if isError(res) || len(gh.calls) != 2 {
		t.Fatalf("calls = %v", gh.calls)
	}
	if gh.calls[0][1] != "ready" || !slices.Contains(gh.calls[1], "--add-label") {
		t.Errorf("the label must go on after the PR is ready: %v", gh.calls)
	}
}

func TestUploadsGitHubOrABrowserWouldRejectAreRefusedBeforeAnythingRuns(t *testing.T) {
	dir := t.TempDir()
	unattached := file(t, "forgot.png", 10)
	cases := map[string]struct {
		codec, input string
	}{
		"vp9 video":       {"vp9", `{"head":"h","title":"t","body":"b","attachments":["` + file(t, "a.webm", 10) + `"]}`},
		"oversize image":  {"h264", `{"head":"h","title":"t","body":"b","attachments":["` + file(t, "big.png", maxImageBytes+1) + `"]}`},
		"relative path":   {"h264", `{"head":"h","title":"t","body":"b","attachments":["shot.png"]}`},
		"missing file":    {"h264", `{"head":"h","title":"t","body":"b","attachments":["` + filepath.Join(dir, "gone.png") + `"]}`},
		"unattached link": {"h264", `{"head":"h","title":"t","body":"see ![x](` + unattached + `)"}`},
	}
	for name, c := range cases {
		gh := &fakeGh{}
		var shown string
		res := authorPoller(&fakeGitHub{}, gh, c.codec).runCreate(context.Background(), call(c.input), approving(true, &shown))
		if !isError(res) || len(gh.calls) != 0 || shown != "" {
			t.Errorf("%s: not refused before approval (calls %v)", name, gh.calls)
		}
	}
}
