package skills

import (
	"slices"
	"strings"
	"testing"
)

const flowHead = "schema: 2\nskill: f\nversion: 1\nwhen: testing flows\nrequires: { tools: [bash], optional_tools: [poll] }\n"

func TestAFlowIsRefusedWhereItsStagesDisagree(t *testing.T) {
	for body, want := range map[string]string{
		"nodes: { a: { say: x, evidence: [{ call: { tool: bash } }] } }\nstages:\n  - a: { checklist: c }\n":                                                                             "not both",
		"stages:\n  - a: { checklist: c, out: { k: [x, y] } }\n  - b: { run: { tool: bash }, when: { field: a.k, equals: z } }\n":                                                        "is not one of",
		"stages:\n  - a: { checklist: c }\n  - b: { run: { tool: bash }, when: { field: a.k, equals: x } }\n":                                                                            "has no out field k",
		"stages:\n  - a: { run: { tool: bash }, on_fail: b }\n  - b: { checklist: c }\n":                                                                                                 "not an earlier stage",
		"stages:\n  - a: { checklist: c }\n  - b: { run: { tool: bash }, max: 2 }\n":                                                                                                     "does not have",
		"stages:\n  - a: { checklist: c, run: { tool: bash } }\n":                                                                                                                        "exactly one of checklist, run, spawn, foreach or repeat",
		"stages:\n  - a: { checklist: c }\n  - b: { run: { tool: bash, input: { command: \"${nope}\" } } }\n":                                                                            "${nope}",
		"stages:\n  - a: { run: { tool: bash, input: { command: \"${b.text}\" } } }\n  - b: { run: { tool: bash } }\n":                                                                   "which is not earlier",
		"stages:\n  - a: { run: { tool: bash } }\n  - b: { run: { tool: bash, input: { command: \"${a.id}\" } } }\n":                                                                     "no capture id",
		"stages:\n  - a: { checklist: c, out: { ids: { type: array, items: { type: string } } } }\n  - e: { foreach: { in: a.ids, as: x }, stages: [{ r: { run: { tool: bash } } }] }\n": "not a list of objects",
		"stages:\n  - a: { checklist: c, out: { k: [x, y] } }\n  - p: { repeat: { max: 2, until: { field: v.k, equals: x } }, stages: [{ r: { run: { tool: bash } } }] }\n":              "which is not earlier",
		"stages:\n  - e: { repeat: { max: 2, until: { field: a.k, equals: x } }, stages: [{ a: { checklist: c, out: { k: [x] } } }, { r: { run: { tool: bash }, on_fail: z } }] }\n":     "not an earlier stage in this list or one around it",
	} {
		_, err := Parse("f", []byte(flowHead+body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\ngave %v, want a problem containing %q", body, err, want)
		}
	}
}

func TestAFlowsGraphLoopsBackSkipsPastAGatedRunAndFailsBackOutOfItsLoop(t *testing.T) {
	s, err := Parse("f", []byte(flowHead+`stages:
  - plan: { checklist: c }
  - rounds:
      repeat: { max: 2, until: { field: check.k, equals: ok } }
      stages:
        - check: { checklist: c, out: { k: [ok, fix] } }
        - fix: { run: { tool: bash }, when: { field: check.k, equals: fix }, on_fail: plan }
        - push: { run: { tool: bash }, when: { field: check.k, equals: fix } }
  - ship: { checklist: c }
`))
	if err != nil {
		t.Fatal(err)
	}
	got := s.StageEdges()
	for _, want := range []Edge{
		{From: "plan", To: "rounds"},
		{From: "rounds", To: "rounds.check"},
		{From: "rounds.push", To: "rounds", Label: "next round"},
		{From: "rounds.check", To: "rounds", Label: "skip", Dashed: true},
		{From: "rounds.fix", To: "plan", Label: "on fail", Dashed: true},
		{From: "rounds", To: "ship", Label: "done"},
	} {
		if !slices.Contains(got, want) {
			t.Errorf("no edge %+v in\n%+v", want, got)
		}
	}
	if slices.ContainsFunc(got, func(e Edge) bool { return e.To == "rounds.push" && e.Dashed }) {
		t.Errorf("a skip lands inside the run of stages it should pass over:\n%+v", got)
	}
}
