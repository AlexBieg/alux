package mcpclient

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// readOnlyHint is the only thing alux reads out of a server's tool annotations, and the
// only use it is put to is the audit that warns when a capability rule admits a writer. So the
// one property that matters is that silence reads as "may write": a server that says nothing,
// or sets annotations without the field, must not be taken for read-only.
//
// What this does not cover is connectServer calling it, which is a single line.
func TestReadOnlyHintTreatsSilenceAsAWrite(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name string
		tool *mcp.Tool
		want bool
	}{
		{"declared read-only", &mcp.Tool{Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, true},
		{"declared not read-only", &mcp.Tool{Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false}}, false},
		{"annotations without the field", &mcp.Tool{Annotations: &mcp.ToolAnnotations{Title: "something"}}, false},
		{"no annotations at all", &mcp.Tool{Name: "x"}, false},
		{"destructive but silent on read-only", &mcp.Tool{Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes}}, false},
		{"nil tool", nil, false},
	} {
		if got := readOnlyHint(tc.tool); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Inventory is what the audit and the tool filter both read, so it has to carry the hint through
// rather than defaulting it somewhere along the way.
func TestInventoryCarriesTheReadOnlyHint(t *testing.T) {
	m := New(nil)
	m.AddForTest("srv", "reader", true)
	m.AddForTest("srv", "writer", false)

	got := map[string]bool{}
	for _, info := range m.Inventory() {
		got[info.Name] = info.ReadOnly
		if info.Server != "srv" {
			t.Errorf("%s: server = %q, want srv", info.Name, info.Server)
		}
	}
	if !got["mcp__srv__reader"] {
		t.Error("the read-only tool lost its hint")
	}
	if got["mcp__srv__writer"] {
		t.Error("the writing tool gained one")
	}
}
