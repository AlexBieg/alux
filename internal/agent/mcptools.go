package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexbieg/alux/internal/mcpclient"
	"github.com/anthropics/anthropic-sdk-go"
)

// McpPrefix namespaces every MCP tool, which is how one dispatch decision separates them from
// everything built in.
const McpPrefix = "mcp__"

// McpFallback returns the handler for any tool name the dispatch table does not know: an MCP
// tool if it is namespaced as one, otherwise bash. bash is the fallback rather than a table
// entry so a model that has been told about the bash tool still reaches it whatever it calls it.
//
// bash is injected because the shell that adopts an overrunning command needs a registry this
// package must not know about. Nil takes RunBash.
//
// Lenient, and only for the main agent. Handing this to a subagent would grant it a shell as a
// side effect of wanting one MCP server, because every unrecognised name lands in bash. Use
// McpHandler there.
func McpFallback(manager *mcpclient.Manager, bash Handler) Handler {
	if bash == nil {
		bash = RunBash
	}
	return func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
		if manager == nil || !strings.HasPrefix(call.Name, McpPrefix) {
			// A misnamed call (a bare "view", say) has no command and would run as an empty one, reading as a success.
			if call.Name != bashTool && commandOf(call) == "" {
				return anthropic.NewToolResultBlock(call.ID,
					"No tool named "+call.Name+" is available to you. Use one of the tools you were given.", true)
			}
			return bash(ctx, call, cb)
		}
		return runMcp(ctx, manager, call, cb)
	}
}

// McpHandler is the strict fallback: MCP tools only, and an error for anything else. It is what
// an agent gets when it should be able to reach a server without that implying a shell.
//
// The distinction is load-bearing rather than tidy. McpFallback's "otherwise bash" is correct for
// the main agent, where bash is deliberately reachable under any name the model invents; in a
// subagent the same line silently converts "give it Linear" into "give it a shell".
func McpHandler(manager *mcpclient.Manager) Handler {
	return func(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
		if !strings.HasPrefix(call.Name, McpPrefix) {
			return anthropic.NewToolResultBlock(call.ID,
				"No tool named "+call.Name+" is available to you. Use one of the tools you were given.", true)
		}
		if manager == nil {
			return anthropic.NewToolResultBlock(call.ID, "No MCP servers are connected.", true)
		}
		return runMcp(ctx, manager, call, cb)
	}
}

// McpTools is the connected MCP tools a rule set permits, which is what an agent should actually
// be offered. Filtering the list rather than only refusing the calls matters: an agent shown
// mcp__linear__update_issue will try it, eat a refusal and spend a turn learning what the tool
// list could have told it for free. The rules stay the single source of truth for both questions.
//
// Nil rules offer everything, which is the main agent.
func McpTools(manager *mcpclient.Manager, rules *Rules) []anthropic.ToolUnionParam {
	if manager == nil {
		return nil
	}
	if rules == nil {
		return manager.Tools()
	}
	permitted := map[string]bool{}
	for _, info := range manager.Inventory() {
		if rules.Permits(info.Name) {
			permitted[info.Name] = true
		}
	}
	var out []anthropic.ToolUnionParam
	for _, def := range manager.Tools() {
		if def.OfTool != nil && permitted[def.OfTool.Name] {
			out = append(out, def)
		}
	}
	return out
}

// AuditMcp reports rules that admit a tool the server itself says is not read-only, one line per
// tool. It is the only use the readOnlyHint is put to, and deliberately so: the hint cannot be
// trusted to *grant* anything, but a server volunteering "this one writes" about a tool the rules
// already allow is worth repeating back to whoever wrote the rules.
//
// So this never changes what an agent may do. It says a rule is broader than its author probably
// meant, which is a thing only a human can resolve.
func AuditMcp(manager *mcpclient.Manager, agentName string, rules *Rules) []string {
	if manager == nil || rules == nil {
		return nil
	}
	var out []string
	for _, info := range manager.Inventory() {
		if info.ReadOnly || !rules.Permits(info.Name) {
			continue
		}
		out = append(out, fmt.Sprintf("%s may call %s, which %s does not report as read-only",
			agentName, info.Name, info.Server))
	}
	return out
}

func runMcp(ctx context.Context, manager *mcpclient.Manager, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	label := "mcp: " + manager.Describe(call.Name, call.Input)
	// Nil-safe because a subagent may be given no approval path at all, where the policy is the
	// whole decision. A missing callback must not read as a decline.
	if cb.Approve != nil && !cb.Approve(ctx, label, "") {
		return declined(call.ID, "tool call")
	}
	if ctx.Err() != nil {
		return interruptedResult(call.ID)
	}

	cb.toolStart(call.ID, label, KindMCP)
	result := manager.Call(ctx, call.Name, call.Input)
	if ctx.Err() != nil {
		return interruptedResult(call.ID)
	}
	cb.toolResult(call.ID, label, result.Display, KindMCP)

	return anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{
		ToolUseID: call.ID,
		Content:   toResultContent(result.Blocks),
		IsError:   anthropic.Bool(result.IsError),
	}}
}

// An MCP tool can return images as well as text, so its result cannot collapse to a string the
// way every built-in tool's can.
func toResultContent(blocks []anthropic.ContentBlockParamUnion) []anthropic.ToolResultBlockParamContentUnion {
	out := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.OfText != nil:
			out = append(out, anthropic.ToolResultBlockParamContentUnion{OfText: block.OfText})
		case block.OfImage != nil:
			out = append(out, anthropic.ToolResultBlockParamContentUnion{OfImage: block.OfImage})
		}
	}
	return out
}
