# Skill flows: draft spec

Status: migration steps 1 to 4 are built. Every skill file declares `schema: 2`; a `schema: 1` file is refused.

## Limitations

- This rests on one live dev-cycle run that stopped at its first coder. Review rounds, push and watch have never run in any form.
- It brings back a state machine between stages, which an earlier decision rejected. Inside a stage, a checklist works the way it does today.
- Runs still don't survive a crash or a restart.
- `finish_stage` is a new tool with a model-written payload. It needs one real API call before it ships.
- A gate proves a question was asked, not that the user agreed. That is accepted for v1.

## Files

One loader reads `~/.config/alux/skills`. A file with `nodes:` is a checklist. A file with `stages:` is a flow. A file with both is refused.

A checklist is today's skill without `step`, `dispatch` or `expand`. A node has `say`, `evidence`, `optional`, `when` and `loop`. Order edges only flag. `per_scenario` goes; per-item checks become a `foreach`.

A flow's checklist stages name checklist files. When a flow starts, alux loads and validates every file it names, and every flow its spawns name.

## Stages

A flow is an ordered list of named stages. alux keeps the cursor.

| Stage | Who acts | Ends when |
|---|---|---|
| `checklist` | the model | the model calls `finish_stage` and every required node has evidence |
| `run` | alux makes one tool call | the call returns, or a poll it started stops |
| `spawn` | alux starts a subagent pinned to a checklist or flow | the subagent's run lands complete |
| `foreach` | alux runs its stages once per item of a list output | every item is done |
| `repeat` | alux runs its stages until a condition holds, at most `max` times | the condition holds; at `max` the run pauses for the user |

`foreach: { in: <stage>.<field>, as: <name> }` runs its `stages:` once per item, one at a time, keyed by each item's `id`; the list must be an `out:` array of objects with a string `id`, and the current item reads as `${<name>.<field>}`. When a reopened stage changes the list, items already done are not redone; only new ids run.

`repeat: { max: <n or ${input}>, until: <condition> }` runs its `stages:` and checks `until` against them after each round. At `max`, alux asks the user through `ask_user` whether to run one more round or fail the run. Inside it, `${<repeat>.round}` is the current round, from 1.

A spawn can set `report: <path>`. When its run lands complete, alux writes the run's final report there itself, so a read-only subagent's findings reach a file. The path must be under `~/.local/state/alux/skills/`, checked when the file loads and again before the write. If the write fails, the spawn counts as not landing complete.

A checklist stage can set `inputs:`, filled with the same refs as a run's input, so a checklist inside a `foreach` sees its item.

### Conditions

`when:` and `until:` take one of three forms: `{ field: <stage>.<name>, equals: <value> }`, `{ input: <name>, equals: <value> }` or `{ available: <tool> }`. alux checks each field against the stage's `out:`, and each input against its type, when the file loads.

### Output

A checklist stage can declare `out:`, a small JSON schema. The model passes the value in `finish_stage`, and alux validates it. A list can set `minItems`, and an `object` field lists its `properties`, all required.

A string field can set `format: command`. `finish_stage` then runs it through the shell rules alux applies before any bash call, without running it, and refuses with the reason if those rules would refuse it. A planner writing a verify alux won't run finds out while it is still planning. A command that runs but is wrong still fails at its `run` stage.

A run stage's result is `${<stage>.text}`. A run can also have `capture:`, regex groups taken from that text, as steps do today.

Later run stages read either as `${<stage>.<field>}`, checked against earlier stages when the file loads. An int capture fills a field that is exactly its ref as a number. A checklist stage binds with the flow's start time, so a `${now_utc}` run directory is the same path in the flow and in every checklist it opens.

### Evidence windows

A checklist stage counts only calls made after it opened. Reopening a stage starts a new window.

A `task` dispatch counts for the stage that made the call, when the call is made; its landing never counts as another call. A later stage that needs a subagent's results checks them with a run stage that reads them (as `pr_findings` does), or uses `spawn`.

A poll belongs to the stage that started it. `finish_stage` is refused while one of that stage's polls is still running.

`finish_stage` must be the only tool call in its message, so no call in the same message can land on either side of the stage edge.

`finish_skill` and `finish_stage` take `waive: [{step, reason}]`. A required step with no evidence that is waived with a non-empty reason counts as closed, in state `waived`. A waiver of a step that already has evidence is ignored. A waiver naming no step of the checklist is refused, and so is one of a step marked `strict: true`. alux keeps every waiver on the run, and the closing report says "finished with N waivers" and lists each as `waived <step>: <reason>`. A checklist stage's waivers are named `<stage>.<step>`, with `[<id>]` after a foreach and `#<round>` after a repeat around it, as in `each[ch-2].review.look`; `finish_stage` also takes `<stage>.<step>`. A waiver for a stage that `on_fail` reopens is dropped, since the stage runs again. A pinned subagent's waivers are listed in its landing, and a spawn stage's are kept on the flow as `<stage>.<step>`, dropped like the flow's own when `on_fail` reopens that stage.

### Failure and reruns

A `run` or `spawn` can name `on_fail: <stage>`, an earlier stage in its own list or in one around it, with an optional `max`. It fires when the call errors or refuses, or when a spawn lands incomplete. alux reopens that stage and tells the model why. Editing a file never triggers a rerun.

Without `on_fail`, at `max`, or with its `on_fail` stage not open, a failure stops the flow at that stage, and alux shows the model the stage's output with three ways on:

- Fix the cause and call `finish_stage`, which runs the stage again.
- Call `finish_stage` with `waive: [{step: <stage>, reason}]`. The stage is marked `waived` and the flow moves on. If later stages read the stage's fields (`${stage.field}`, `when`, `repeat.until`, `foreach`), `out` must give those fields by name, each of the type it reads as: `text` a string, an int capture an integer. alux refuses, naming the missing or mistyped ones, otherwise.
- Call `fail_run`.

A run or spawn stage with `strict: true` cannot be waived: its failure fails the run.

### Gates

A gate is a checklist stage whose evidence includes `ask_user`. Nothing after it starts until the model finishes that stage, and its window means an earlier stage's question doesn't count.

## Examples

coder-task:

```yaml
stages:
  - work: { checklist: coder-work, out: { outcome: [changed, nothing_to_do], why: string } }
  - verify: { run: { tool: bash, input: { command: "${verify}" } }, on_fail: work, max: 3 }
  - described:
      run: { tool: bash, input: { command: "jj log -r @ ..." } }
      when: { field: work.outcome, equals: changed }
      on_fail: work
```

dev-cycle, in outline:

```yaml
stages:
  - intake: { checklist: dev-intake }   # brief, code, questions, spec
  - plan:   { checklist: dev-plan, out: { changes: [...] } }   # evidence includes ask_user
  - base:   { checklist: dev-base }
  - each:
      foreach: { in: plan.changes, as: change }
      stages:
        - code:   { spawn: { agent: coder, run: coder-task, inputs: {...} }, on_fail: base }
        - land:   { checklist: dev-land }
        - rounds:
            repeat: { max: "${max_rounds}", until: { field: verdict.kind, equals: clean } }
            stages:
              - review:  { spawn: { agent: research, run: change-review } }
              - verdict: { checklist: dev-verdict, out: { kind: [clean, fix], task: string } }
              - fix:     { spawn: { agent: coder, run: coder-task, change: "${change.id}" }, when: { field: verdict.kind, equals: fix } }
  - ship: { checklist: dev-ship }   # prefs, bodies, push, open
  - watch:
      repeat: { max: 2, until: { field: check.kind, equals: green } }
      stages:
        - check:  { checklist: dev-watch, out: { kind: [green, fix], change: string, task: string } }
        - fix:    { spawn: { agent: coder, run: coder-task, change: "${check.change}" }, when: { field: check.kind, equals: fix } }
        - repush: { checklist: dev-repush, when: { field: check.kind, equals: fix } }
```

## Runtime

- A new `internal/flow` package owns the cursor, `finish_stage`, output, windows and stage kinds. `skillrun.Host` keeps checking checklist stages.
- alux moves the cursor itself when a `run` returns or a spawn lands, not inside another call's after-hook. It uses the approvals of whoever started the flow.
- A run stage's output is the result of `finish_stage` or of the landing that set it off, never text appended to an unrelated call.
- Step call ids keep the `alux_step_` prefix and bash's exit-status check. A run whose whole command is an input still asks for approval.
- A spawn starts its subagent in the background and the model ends its turn; `finish_stage` is refused while it runs. Its landing moves the flow on at the start of the next turn, which the landing's wake-up starts, or in the result of a `finish_stage` called before then. A spawn's inputs and `change` take the same `${<stage>.<field>}` refs as a run's input.
- A pinned subagent can run a flow: `finish_stage` joins its allowed tools, and its landing carries the flow's output.
- Ending or dropping a flow cancels the subagents it started.

## Migration, smallest first

1. Build `flow` with `checklist` and `run` stages, `out:` with `format: command`, and `finish_stage`. Port smoke and coder-task.
2. Add run output and `capture:`. Have `pr_findings` refuse while a persona has no submission. Port pr-ship and pr-review.
3. Add `spawn`, `foreach` and `repeat`. Port e2e-verify, then dev-cycle.
4. Remove `step`, `dispatch`, `expand`, captures on nodes and `per_scenario` from checklists. Bump the schema.

Deferred: persistence of the cursor, model-started flows, and a `grant:` block (`requires.tools` keeps preapproving for now).
