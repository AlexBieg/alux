package mcpclient

import "testing"

func TestMainToolsDefersOnlyNamedServersAndLeavesToolsUntouched(t *testing.T) {
	m := New(nil)
	notes := m.AddForTest("notes", "list", true)
	linear := m.AddForTest("linear", "get", true)

	deferred := map[string]bool{}
	for _, def := range m.MainTools([]string{"notes"}) {
		deferred[def.OfTool.Name] = def.OfTool.DeferLoading.Value
	}
	if !deferred[notes] || deferred[linear] {
		t.Fatalf("MainTools deferred = %v, want only %s", deferred, notes)
	}

	for _, def := range m.Tools() {
		if def.OfTool.DeferLoading.Valid() {
			t.Fatalf("Tools() carries defer_loading on %s; subagents have no search tool to reach it", def.OfTool.Name)
		}
	}
}
