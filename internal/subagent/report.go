package subagent

import (
	"fmt"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/mcpclient"
	"github.com/alexbieg/alux/internal/report"
)

// AuditTools reports where the capability rules and the connected servers disagree, one line per
// finding and empty when there is nothing to say. Two directions, because a hand-written
// allow-list can be wrong both ways:
//
//   - Too broad: an agent's rules admit a tool the serving server does not itself call read-only.
//   - Too narrow: a server is connected that no agent's rules reach at all.
//
// Callable only once the servers have connected, since the inventory is empty before that. It
// changes nothing in either direction — the readOnlyHint is advisory by specification and cannot
// decide what an agent may reach, and a server nobody wired up may well be one no subagent should
// have. Both are things only the person who wrote the rules can settle.
func (m *Manager) AuditTools(mcp *mcpclient.Manager) []string {
	var out []string
	for _, name := range m.order {
		def := m.defs[name]
		if def.Policy == nil {
			continue
		}
		out = append(out, agent.AuditMcp(mcp, name, def.Policy)...)
	}
	return append(out, m.auditUnreached(mcp)...)
}

// auditUnreached reports every connected server no gated agent can call anything on. It is the
// converse of AuditMcp and exists because the coder's allow-list is enumerated by name: a server
// added to the config is simply invisible to every subagent until someone writes a rule for it,
// and nothing else says so. The main agent is ungated, so its own session looks entirely normal
// while every subagent silently lacks the lookup.
//
// Per server rather than per tool: a thirty-tool server would otherwise bury the finding it is
// trying to make. A server some of whose tools are reachable is not reported at all — that is the
// documented cost of enumerating, not a mistake.
func (m *Manager) auditUnreached(mcp *mcpclient.Manager) []string {
	if mcp == nil {
		return nil
	}
	var gated []*agent.Rules
	for _, name := range m.order {
		if def := m.defs[name]; def.Policy != nil {
			gated = append(gated, def.Policy)
		}
	}
	// With nothing gated there is nothing to be missing from: every server would be reported, and
	// the finding would say more about the roster than about the servers.
	if len(gated) == 0 {
		return nil
	}

	tools := map[string]int{}
	reached := map[string]bool{}
	var servers []string
	for _, info := range mcp.Inventory() {
		if _, seen := tools[info.Server]; !seen {
			servers = append(servers, info.Server)
		}
		tools[info.Server]++
		for _, rules := range gated {
			if rules.Permits(info.Name) {
				reached[info.Server] = true
				break
			}
		}
	}

	var out []string
	for _, server := range servers {
		if reached[server] {
			continue
		}
		out = append(out, fmt.Sprintf("%s is connected, but no subagent has a rule reaching any of its %d tool%s",
			server, tools[server], plural(tools[server])))
	}
	return out
}

// Catalogue is what each registered agent may do, which is the half of /agents that does not
// depend on anything having run yet. It exists because a capability gate nobody can read is one
// nobody can check: the rules live in code, and this is where they are stated in the open.
func (m *Manager) Catalogue() string {
	if len(m.order) == 0 {
		return ""
	}
	var b strings.Builder
	for _, name := range m.order {
		def := m.defs[name]
		mode := "foreground"
		if def.Background {
			mode = "background"
		}
		fmt.Fprintf(&b, "  %s  ·  %d turns, %s\n", name, def.steps(), mode)
		if def.Policy == nil {
			// Not a gap. An agent whose handlers are already its whole limit needs no rules, and
			// saying so is more useful than printing an empty table.
			fmt.Fprintf(&b, "      limited by its tools alone\n")
		} else {
			for _, line := range def.Policy.Summary() {
				fmt.Fprintf(&b, "      %s\n", line)
			}
		}
		// A second agent is a second bill, and it is the one thing about a pair that its rules do
		// not say: the peer runs under its own definition, so nothing in the summary above hints
		// that dispatching this agent starts two conversations rather than one.
		if def.Peer != nil {
			fmt.Fprintf(&b, "      pairs with %s: plans first, reviews every %d editing steps and at the end, up to %d steers and %d questions\n",
				def.Peer.Def.Name, checkpointEvery, def.Peer.steers(), def.Peer.rounds())
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ReportTable is what /agents shows: every run this session has dispatched, one row each.
func ReportTable(runs []Run) report.Table {
	t := report.Table{
		Head:  []string{"agent", "model", "state", "took", "name"},
		Empty: "  No subagents have run in this session.",
	}
	if len(runs) == 0 {
		return t
	}

	live := 0
	for _, run := range runs {
		if run.Live() {
			live++
		}
		// A live run's figure is where it has got to; a finished one's is what it cost.
		took := short(run.Elapsed())
		if run.Live() {
			took = fmt.Sprintf("%d/%d", run.Step, run.MaxSteps)
		}
		what := report.Cell{Text: run.Label()}
		if run.State == Failed {
			what = report.Cell{Text: "failed — " + firstLine(run.Err), Tone: report.Bad}
		} else if run.Live() && run.Activity != "" {
			what.Text = run.Activity
		}
		t.Rows = append(t.Rows, report.Row{Cells: []report.Cell{
			{Text: run.Agent}, {Text: shortModel(run.Model), Tone: report.Muted},
			{Text: string(run.State), Tone: stateTone(run.State)}, {Text: took, Tone: report.Muted}, what,
		}})
	}

	t.Footer = fmt.Sprintf("%d run%s this session, %d still going.", len(runs), plural(len(runs)), live)
	return t
}

func stateTone(s State) report.Tone {
	switch s {
	case Done:
		return report.OK
	case Running:
		return report.Active
	case Failed:
		return report.Bad
	case Waiting, Idle:
		return report.Warn
	}
	return report.Muted
}

func shortModel(model string) string { return strings.TrimPrefix(model, "claude-") }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
