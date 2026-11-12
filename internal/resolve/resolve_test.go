package resolve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/alexbieg/alux/internal/mcpclient"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestIDsMatchTheThreeShapesAndNothingNearThem(t *testing.T) {
	text := "See #4321, ABC-667 and ent_0123456789abcdef; not #123, &#12345;, a/#12345, ent_xyz or abc-1."
	want := []string{"#4321", "ABC-667", "ent_0123456789abcdef"}
	if got := IDs(text); !slices.Equal(got, want) {
		t.Fatalf("IDs = %v, want %v", got, want)
	}
}

func TestAMissIsLookedUpOnceThenKnown(t *testing.T) {
	var calls atomic.Int32
	var named atomic.Int32
	r := &Resolver{
		Repo: "acme/web",
		Gh: func(_ context.Context, args ...string) (string, error) {
			calls.Add(1)
			if !slices.Equal(args, []string{"pr", "view", "4321", "--repo", "acme/web", "--json", "title"}) {
				t.Errorf("gh %v", args)
			}
			return `{"title":"Make export rows optional"}`, nil
		},
		OnName: func() { named.Add(1) },
	}
	for range 3 {
		if _, ok := r.Name("#4321"); ok {
			t.Fatal("a first miss reported a name")
		}
	}
	r.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("gh ran %d times, want 1", n)
	}
	if name, ok := r.Name("#4321"); !ok || name != "Make export rows optional" || named.Load() != 1 {
		t.Fatalf("Name = %q, %v after %d callbacks", name, ok, named.Load())
	}
}

func TestAFailedLookupIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	r := &Resolver{Repo: "acme/web", Gh: func(context.Context, ...string) (string, error) {
		calls.Add(1)
		return "", os.ErrNotExist
	}}
	r.Name("#4242")
	r.Wait()
	r.Name("#4242")
	r.Wait()
	if calls.Load() != 1 || r.Known()["#4242"].State != None {
		t.Fatalf("%d calls, entry %+v", calls.Load(), r.Known()["#4242"])
	}
}

func TestARemoteResolverAsksOnlyTheSession(t *testing.T) {
	var named atomic.Int32
	r := &Resolver{
		Repo:   "acme/web",
		Gh:     func(context.Context, ...string) (string, error) { t.Error("a remote resolver ran gh"); return "", nil },
		Remote: func(_ context.Context, id string) string { return "named " + id },
		OnName: func() { named.Add(1) },
	}
	r.Name("#4321")
	r.Wait()
	if name, ok := r.Name("#4321"); !ok || name != "named #4321" || named.Load() != 1 {
		t.Fatalf("Name = %q, %v after %d callbacks", name, ok, named.Load())
	}
}

func TestLookupAnswersALearnedNameWithoutAskingAgain(t *testing.T) {
	r := &Resolver{Repo: "acme/web", Gh: func(context.Context, ...string) (string, error) {
		t.Error("gh ran for a name already learned")
		return "", nil
	}}
	r.Learn("#4242", "Fix the importer")
	if got := r.Lookup("#4242"); got != "Fix the importer" {
		t.Fatalf("Lookup = %q", got)
	}
}

type fakeMCP struct {
	tools []mcpclient.ToolInfo
	defs  []anthropic.ToolUnionParam
	calls []string
	reply string
}

func (f *fakeMCP) Inventory() []mcpclient.ToolInfo   { return f.tools }
func (f *fakeMCP) Tools() []anthropic.ToolUnionParam { return f.defs }
func (f *fakeMCP) Call(_ context.Context, name string, input json.RawMessage) mcpclient.Result {
	f.calls = append(f.calls, name+" "+string(input))
	return mcpclient.Result{Display: f.reply}
}

func TestTicketKeysAskTheConfiguredServersReadOnlyIssueTool(t *testing.T) {
	mcp := &fakeMCP{
		tools: []mcpclient.ToolInfo{
			{Name: "mcp__linear__get_issue", Server: "linear", ReadOnly: true},
			{Name: "mcp__linear__update_issue", Server: "linear"},
		},
		defs: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
			Name:        "mcp__linear__get_issue",
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"issueId": map[string]any{"type": "string"}}},
		}}},
		reply: `{"identifier":"ABC-667","title":"Make export rows optional"}`,
	}
	unset := &Resolver{MCP: mcp}
	unset.Name("ABC-667")
	unset.Wait()
	if got := unset.Known()["ABC-667"]; got.State != None || len(mcp.calls) != 0 {
		t.Fatalf("with no ticket server: entry %+v, calls %v", got, mcp.calls)
	}
	r := &Resolver{MCP: mcp, TicketServer: "linear"}
	r.Name("ABC-667")
	r.Wait()
	if got := r.Known()["ABC-667"]; got.Name != "Make export rows optional" {
		t.Fatalf("entry %+v", got)
	}
	if !slices.Equal(mcp.calls, []string{`mcp__linear__get_issue {"issueId":"ABC-667"}`}) {
		t.Fatalf("calls %v", mcp.calls)
	}
}

func TestTicketKeysPreferTheVaultAndSkipQuietlyWithNoTool(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, "Tickets", "ABC-667 Make export rows optional"), 0o755); err != nil {
		t.Fatal(err)
	}
	mcp := &fakeMCP{tools: []mcpclient.ToolInfo{{Name: "mcp__github__get_issue", Server: "github", ReadOnly: true}}}
	r := &Resolver{Vault: vault, TicketsDir: "Tickets", TicketServer: "linear", MCP: mcp}
	r.Name("ABC-667")
	r.Name("UTF-8")
	r.Wait()
	if got := r.Known()["ABC-667"]; got.Name != "Make export rows optional" {
		t.Fatalf("vault entry %+v", got)
	}
	if got := r.Known()["UTF-8"]; got.State != None || len(mcp.calls) != 0 {
		t.Fatalf("entry %+v, calls %v", got, mcp.calls)
	}
}
