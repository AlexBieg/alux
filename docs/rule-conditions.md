# Rule conditions (CEL `when` on hold rules)

Status: built, both rollout steps done. A wrong condition silently turns a hold off, so no condition takes effect until it has been replayed against recorded sessions.

## What changes

A hold rule may carry `attrs.when`, a CEL expression. It is evaluated on each call the rule's trigger matches.

| Rule | First matching call | Later matching calls |
|---|---|---|
| hold, no `when` (today) | held; rule shown after the turn | run |
| hold, `when` false | runs; rule shown as advice | run |
| hold, `when` true | held, with the condition and evidence | held again while `when` stays true, unless the call is an identical retry of one already held |

So a rule with `when` is advice by default and a hold only when it is broken. The identical-retry exception keeps an override: the agent reads the evidence, and if it still thinks the call is right it makes the same call again.

Advise rules never take `when`. Early, imprecise advice is what changed behaviour in the replay; narrowing it would make it arrive late.

## The language

Variables:

- `call.tool` (string), `call.args` (map of the tool input), `call.commands` (list of shell commands as the harness normalises them for triggers), `call.path` (file read or edited, relative to its repo).
- `session.cwd`, `session.repo` (repo entity id, as triggers resolve it), `session.agent` ("" for the main agent).

Functions, a closed set owned by Go:

| Version | Function | For |
|---|---|---|
| v1 | `words(s)`, `cmd(c, prefix)`, `hasFlag(c, flag)`, `positional(c)` | PR body length, a setup script, jj git init |
| v2 | `broadRoot(path)` | broad-root grep back to a hold |
| v2 | `descendants(rev)`, `listed(rev)`, `revs(c)` | jj stacked back to a hold |

A function that shells out (`descendants`) is cached per call and must be cheap enough to run on every matching call.

## Where it lives

- A new package `internal/rulecel` holds the environment and functions. Both `records` and `agent` import it.
- `records` compiles and type-checks `when` when a rule is written. An expression that does not compile, or is set on an advise rule, is refused with CEL's error.
- `agent` compiles each condition once, keyed by rule id, and evaluates it in `holdForRules`.
- The `vault_record` description lists the variables and functions, so an agent can write a condition.
- A runtime error (a missing arg key, say) counts as false: the call runs and the error is logged. A broken condition fails open, like a rule with no hold.

## Hold message

Today: "Not run: a rule must be read before this call." With `when` the message also carries the condition and what made it true, for example "3 changes sit on qvsvoxko and no earlier jj log in this conversation listed them." Evidence comes from `broadRoot`, `descendants` and `listed`, and leaves out a revision with nothing on it or one already listed. v1 functions return no evidence, so v1 messages carry only the condition. The replay prints the same evidence under each held call.

## Replay before use

A gated test (extending `cel_probe_test.go`) takes a rule id and a candidate `when`, and runs it over every recorded call the rule's trigger matches. It prints matched / would hold / would run / errors, and each held call in full. `ALUX_CEL_TRIGGER` replays a candidate trigger list in place of the stored one. Sessions record no cwd, so offline a relative path never counts as broad. `descendants` reads the repo as it is now, so the replay stubs it to one change and tests `listed` alone.

The procedure: run the replay, show the user the held calls with `ask_user`, and write the condition only on their yes. Replay shows what a condition holds, not what it misses, so for a rule with few triggers read the passed calls too. The store does not enforce any of this.

## Rollout

1. v1 functions: conditions on command shape, flags and word counts.
2. v2: `broadRoot`, `descendants`, `listed`: conditions on broad-root searches and stacked jj changes.

## Tests

- A hold rule whose `when` is false runs the call and shows the rule (red if the condition is ignored).
- A true `when` holds every time, and an identical retry runs (red if the override is lost).
- `vault_record` refuses a `when` that does not compile, or one on an advise rule (red if validation is skipped).
- A runtime error runs the call (red if errors fail closed).

Hold rules without `when` keep today's behaviour: their condition is the trigger itself.
