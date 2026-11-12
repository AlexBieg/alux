# Skill call predicates and order notices

Status: built. Progress notices replace the call-matched notices built first, which guessed the agent's step and fired falsely wherever two nodes share a tool. A probe shows which past calls a predicate would count, but it cannot replay a whole skill run, because run logs are not saved. The main transcript also does not record which calls failed, so the probe counts failed calls a live run would not.

## What changes

1. A `call` evidence rule replaces `tool_called`. It names the tools and, optionally, a CEL condition over the call.
2. When the checker marks a node out of order mid-run, the agent is told at the end of that turn. Nothing guesses which step the agent is on, and order edges still never refuse anything.
3. The rule-condition probe learns to replay a skill predicate before it goes into a skill file.

## The `call` rule

```yaml
- call: { tool: pr_detail }
- call: { tool: [pr_create, pr_edit] }
- call: { tool: bash, when: 'call.commands.exists(c, cmd(c, "jj diff") && hasFlag(c, "-r"))' }
- call: { tool: browser, when: 'call.args.action == "open" && call.args.url.startsWith(inputs.app_url)' }
- call: { tool: web_search, min: 3, distinct: query }
```

- `tool` is a name or a list. It stays a plain field, so the loader can still check every tool against `requires` and `optional_tools` without reading the expression.
- `when` is a CEL bool, compiled with the hold-rule environment from `internal/rulecel`: `call.*`, `cmd`, `hasFlag`, `positional`, `revs`, `words`. `session` and the functions that read the session (`broadRoot`, `listed`, `descendants`) are left out. Evidence is about what the agent did, not about the repo as it is now.
- Three more variables: `inputs` (the run's bound inputs), `vars` (the skill's vars, expanded), and `scenario` (the scenario's fields, only in an expand's template nodes). The values are passed in as data, never pasted into the expression text, so a plan or an input cannot inject CEL.
- `inputs`, `vars` and `scenario` are read only as `x.name`, `has(x.name)` or `x["literal"]`, so the loader can check every key. A string input starting `~/` is passed in expanded, as vars are.
- `revs(c)` is `["@"]` for a jj command that names no revision, so "names a revision" is `revs(c) != ["@"]`.
- `min` and `distinct` keep their meaning.
- A call where `when` errors does not count. The node's missing line shows the first error.

Load-time checks:

- `when` compiles and is a bool.
- Every `inputs.<name>` and `vars.<NAME>` it selects is declared, and `scenario.*` appears only in template nodes. This needs a walk over the checked expression, because CEL cannot type map keys.
- `${...}` inside `when` is refused, so there is one way to refer to an input.

Missing line, as today but with the condition: `bash where call.commands.exists(...): 0 of 1 calls`.

## Line rules and out conditions

```yaml
- artifact_lines:
    path: ${RUN}/findings-${id}.md
    section: "## round ${round}"
    pattern: '^\[blocking\]\s+(\S+?:\d+)|^\[non-blocking\]\s+(\S+?:\d+).*\(overruled:\s*[^)\s][^)]*\)'
    covers: { path: "${RUN}/review-${id}-${round}.md", pattern: '^\[blocking\]\s+(\S+?:\d+)' }
```

- `min` is optional and defaults to 0. `max` is unmet when more lines match, and its missing line lists them.
- `section` counts only the lines after the line equal to it, up to the next heading of the same or a higher level. A file without that line is unmet.
- `covers` keys each matching line of its file by its first non-empty group. Each key must be the key of a counted line. The missing line lists the uncovered lines. The rule's own pattern needs a group too.
- A node's `when: { out: <field>, equals: <value> }` (or `in:`) reads the out that `finish_stage` gives. The node is skipped everywhere else, and while it is unmet the refusal ends with its `say`. Nothing checks the field against the flow's `out:` at load, because a checklist does not know the stage that opens it.

## Migration

The builtin skills use `tool_called` about 60 times. Most name only a tool and become `call: { tool: x }`. The `input`/`input_prefix` ones become `when` conditions. pr-ship's `open` becomes `tool: [pr_create, pr_edit]`. `tool_called` is removed in the same change, along with its schema entry. The schema stays at version 1, because every skill file is in this repo.

## Progress notices

A skill is a checklist checked over the whole run. The checker already marks a node `out of order` when its evidence is complete while a node before it is not done (`checkOrder`). Today the agent learns that only when `finish_skill` is refused. A progress notice tells it at the end of the turn in which it happened.

- At the end of each turn, the `Agent.SkillNotices` hook runs `skills.Check` on the run's log. A notice is due for each node that is done while a node before it is still pending, as `checkOrder` judges it.
- Only that reason notifies. `checkOrder`'s other reason, "done before X finished", compares done times, and a file rule's done time is the last call naming its file, views included. A later read or a shared file's append moves it, so that reason fires and flickers in correctly ordered runs.
- One line per node, naming every pending predecessor:

  > Skill pr-ship: `open` counts as done, but `body` comes before it and is still pending. Do it now, then redo `open` if it depended on it.

  A pending predecessor alux runs (a `step` or `dispatch` node) is named apart: "`verify` is run by alux once the nodes before it are done."
- Once per node and pending predecessor pair, so a node that later falls behind a different step is told again. A pair is marked sent only when the notice is delivered, so a cancelled turn loses nothing.
- It is computed once per turn, after every call and every step alux ran in it, so a batch of calls needs no special handling.
- It travels in the nonce-marked `<skill-notice>` block, with the hook and provenance text already built.
- `skills.Notices` and its call matching are removed. Nothing guesses which step the agent is on.

A notice is only as right as the skill's evidence. A wrong one means a node's evidence was met by a call that belongs to another step, and the fix is to that skill's evidence. Known now: pr-ship's and dev-cycle's `hand-off` nodes count `pr_detail` and `pr_create`, calls that `watch` and `open` make. Both are optional "tell the user" nodes no call marks, so they are removed and their `say` text moves into `watch` in both skills (dev-cycle's `fixes` is an expand node, which takes no `say`, and a green round never reaches `rewatch`). pr-ship's `watch` gains `call: { tool: pr_detail }`, which its text already asks for. change-review's `report` counts views the `diff` step makes before `spec` is read, so its view rule leaves out `inputs.spec` and `inputs.criteria`.

## Probe

`TestCelProbe` gains a skill mode:

```
ALUX_CEL_PROBE=1 ALUX_CEL_TOOL=bash ALUX_CEL_WHEN='...' ALUX_CEL_INPUTS='{"app_url":"http://localhost:3000"}' go test ./internal/agent -run TestCelProbe
```

It runs the condition over every recorded call to that tool and prints:

| with the tool | would count | would not | errors |
|---|---|---|---|

Then it lists each call that would not count, in full. For evidence, the calls a predicate misses are what break a run, which is the reverse of a hold. It also lists the counted calls when there are few of them.

Procedure: replay each new or changed `when` before it goes into a skill file, and read the misses.

## Tests

- A `call` rule with `when` counts a matching call and skips a non-matching call of the same tool. This goes red if `when` is ignored.
- A `tool` list counts any listed tool. This goes red if only the first tool is read.
- An input is read as data. An input whose value is CEL text never changes what counts. This goes red if values are pasted into the expression.
- The loader refuses an undeclared `inputs.x`, `${...}` in `when`, and `scenario.*` outside a template node.
- A turn in which a node becomes done while a predecessor is pending ends with one notice naming both. A later turn adds none for that pair, and a new pending predecessor gets its own. This goes red if notices never show, repeat, or are keyed by node alone.
- Viewing an earlier node's file after a later node is done gives no notice. This goes red if the "finished" reason notifies.
- A node whose only pending predecessor is optional and never done gets no notice. This goes red if the look-through is lost.
- A pending `step` predecessor is worded as run by alux. This goes red if it is told to the agent to do.
- A notice computed in a cancelled turn is sent in the next turn. This goes red if a pair is marked sent before delivery.
- In one turn, a call that completes a node ahead of its predecessor and a later call that completes the predecessor give no notice, because the turn's end sees both done. This goes red if notices are computed per call.
- The notice arrives as a nonce-marked text block after the tool results, and no tool result contains it. This goes red if it is appended to the result instead.

## Later

- Save each skill run's event log next to its run directory. That makes full-run replay possible, and it also covers crash persistence.
- Convert node `when` (`available`, `input`, `scenario_field`) to CEL once evidence has settled.
