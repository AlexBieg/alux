package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/alexbieg/alux/internal/vault"
	"github.com/anthropics/anthropic-sdk-go"
)

// The vault's two read tools live here rather than in the vault package so that package stays
// free of the agent's callback and dispatch types, and can be tested without one.

// VaultTools is empty when no vault is configured, so the feature disappears rather than
// half-working.
func VaultTools() []anthropic.ToolUnionParam {
	if vault.Root() == "" {
		return nil
	}
	notePath := map[string]any{"type": "string", "description": "Path relative to the vault root, e.g. Systems/Foo.md"}
	strict := func(name, description string, properties map[string]any, required []string) anthropic.ToolUnionParam {
		return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        name,
			Description: anthropic.String(description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties:  properties,
				Required:    required,
				ExtraFields: map[string]any{"additionalProperties": false},
			},
		}}
	}
	return []anthropic.ToolUnionParam{
		strict("vault_search", "Ranked full-text search across the knowledge vault, over note sections rather than whole notes. Ask a question in words — words that never appear on the same line still match. Returns the best-matching sections as 'path › heading' with a snippet of each.",
			map[string]any{"query": map[string]any{"type": "string", "description": "What you want to know, in words. A question works better than a single keyword."}}, []string{"query"}),
		strict("vault_read", "Read a note from the knowledge vault, or one section of it. A long read comes back in pages of whole lines, each ending with the offset that continues it.",
			map[string]any{"path": notePath, "section": map[string]any{
				"type":        "string",
				"description": "Heading of the single section to read, exactly as vault_search reported it. Omit to read the whole note.",
			}, "offset": vault.OffsetArg}, []string{"path"}),
	}
}

// VaultHandlers is merged into the dispatch table by BuiltinHandlers in builtin.go, and into the
// research subagent's by its own toolset.
func VaultHandlers() map[string]Handler {
	if vault.Root() == "" {
		return nil
	}
	return map[string]Handler{
		"vault_search": runVaultRead,
		"vault_read":   runVaultRead,
	}
}

// Reading the vault cannot break anything and asking about it would be noise, so the two read
// tools skip the approval prompt.
func runVaultRead(_ context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Query   string `json:"query"`
		Path    string `json:"path"`
		Section string `json:"section"`
		Offset  int    `json:"offset"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}

	// The main agent reads the configured vault, which is also the one its tools were built for
	// — VaultTools returns nothing when there is none, so this is never empty here.
	root := vault.Root()
	label := "vault: search " + in.Query
	if in.Path != "" {
		label = "vault: read " + in.Path
		if in.Section != "" {
			label += " › " + in.Section
		}
		if in.Offset > 1 {
			label += fmt.Sprintf(" from line %d", in.Offset)
		}
	}
	cb.toolStart(call.ID, label, KindVault)

	var output string
	var err error
	if in.Path != "" {
		output, err = vault.Read(root, in.Path, in.Section, in.Offset)
	} else {
		output, err = vault.Search(root, in.Query)
	}
	if err != nil {
		cb.toolResult(call.ID, label, err.Error(), KindVault)
		return failed(call.ID, err)
	}
	cb.toolResult(call.ID, label, output, KindVault)
	return ok(call.ID, output)
}
