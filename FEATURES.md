# alux features

A catalogue of what the harness does today, grouped by area. Package names
are under `internal/` unless shown otherwise. For setup and configuration
keys, see [README.md](README.md).

- [Command line](#command-line)
- [Agent loop](#agent-loop)
- [Tools the model can call](#tools-the-model-can-call)
- [Shell and background processes](#shell-and-background-processes)
- [Safety and permissions](#safety-and-permissions)
- [Context and compaction](#context-and-compaction)
- [Sessions](#sessions)
- [Subagents](#subagents)
- [Skills and flows](#skills-and-flows)
- [Workers and workspaces](#workers-and-workspaces)
- [jj integration](#jj-integration)
- [Pull requests](#pull-requests)
- [Code review](#code-review)
- [Record store](#record-store)
- [Vault](#vault)
- [Vault-to-record migration](#vault-to-record-migration)
- [Background jobs](#background-jobs)
- [Workflow mining](#workflow-mining)
- [MCP](#mcp)
- [Web](#web)
- [Browser](#browser)
- [Calendar](#calendar)
- [Terminal UI](#terminal-ui)
- [Slash commands](#slash-commands)
- [Notifications](#notifications)
- [Developer tools](#developer-tools)
- [Extras](#extras)

## Command line

`cmd/alux`

| Invocation | What it does |
| --- | --- |
| `alux` | Starts a session server process and attaches this TUI to it |
| `alux attach` | Attaches to an existing served session |
| `alux stop` | Stops a detached session |
| `alux skills validate [file.yaml…]` | Validates skill files (exit 0 valid, 1 invalid, 2 usage/IO error) |
| `--here` | Runs in this checkout instead of claiming a pool workspace |
| `--serve` | Runs headless, serving the session to whichever TUI attaches |
| `--inline` | Runs the session inside this TUI; it ends when the TUI quits |
| `--worker KEY --mode --prompt --base` | Runs a headless worker with a mode, a first message and a start point |

- **Pool-slot claiming**: at startup alux moves into its own jj pool
  workspace, keeping the relative subdirectory.
- **Detached sessions** (`attach`): a session is its own process that
  serves TUIs over a unix socket. One attached TUI drives and the others
  view; a viewer takes over on its first action that changes the session.
- **Control socket**: a pane that isn't attached can read a session's
  parked prompt, answer it, send a message, or stop the session.
- **API key** from `ANTHROPIC_API_KEY` or `~/.config/alux/auth.json`, with a
  warning if the file is group- or world-readable. `ALUX_MODEL`,
  `ALUX_VAULT_MODEL` and `ALUX_ALIAS_MODEL` choose models (`llm`).
- **Context window discovery**: `ALUX_CONTEXT_WINDOW`, else the window the
  API reports, else a fallback (`llm`).

## Agent loop

`agent`

- **Streaming turn loop**: streams the reply and runs the tools it asks for.
  A turn ends as done, cancelled, out of steps, or with the context full (90%
  of the window).
- **Concurrent tool calls**: tools marked concurrent run in parallel; the
  rest run in order.
- **One choke point**: every tool call from every agent goes through the
  same gate, which runs rule holds, policy, guardian and approval.
- **Server-side context editing**: old thinking is cleared on the server;
  tool-result clearing is applied to subagents only.
- **Project instructions**: `AGENTS.md`/`CLAUDE.md` are folded into the
  system prompt. `agents_md_exclude` drops named sections and warns when a
  named heading no longer exists.
- **Vault census in the system prompt**: folder counts (~130 tokens) instead
  of a note listing, plus notes matching the ticket branch or repo.
- **Standing rules** from the record store load at startup and fire again on
  matching tool calls (see [Safety](#standing-rules)).
- **Finalize**: a subagent that runs out of budget gets one tool-less turn to
  report what it left unfinished.
- **Secret scrubbing** of every tool result and transcript line (see
  [Safety](#secret-scrubbing)).

## Tools the model can call

Some tools are offered only when their feature is configured.

| Tool | What it does | Package |
| --- | --- | --- |
| `bash` | Persistent shell; `sandbox`, `restart`, `head`/`tail`, `seconds` (held, not killed, past the limit) | agent, procs |
| `str_replace_based_edit_tool` | View (files, directories, images), create, str_replace, insert | agent, tools |
| `glob` | Find files by name via fd; sorted and capped; `hidden` | tools |
| `grep` | ripgrep with `output_mode`, `-i -A -B -C -o`, `multiline`, `head_limit`, `glob`, `type`, `hidden` | tools |
| `compose` | Several known tool calls in order in one request, each still checked | agent |
| `ask_user` | One question or a series of up to 6, with options, context, free text and `when` branches | agent, ask |
| `decline_suggestion` | Reject a suggested tool in favour of a blocked command, putting it to the user | agent |
| `read_rule` | Read rules by topic or id | agent |
| `run_background` | Start a `finite` or `ongoing` process with `check_in`, `until`, `repeat` | procs |
| `bash_output` | Read new output from a background process | procs |
| `kill_shell` | Stop a background process and its children | procs |
| `background_proc` | Background a held command, or re-arm a process's wake | procs |
| `poll` | Rerun a check until `until`/`while`/exit 0; retries on network loss, checks on wake | procs |
| `recall` | Expand compacted history steps by number or range | session, compact |
| `task` | Dispatch a subagent | subagent |
| `agent_status`, `agent_cancel` | Inspect or stop subagent runs | subagent |
| `agent_reply`, `agent_extend` | Answer a run's question; grant or refuse more budget | subagent |
| `ask_main` | (subagents) Ask the main agent a question | subagent |
| `ask_navigator` | (pair driver) Ask its navigator | subagent |
| `land_run` | Land or drop a coder run's change | subagent |
| `start_skill`, `finish_skill`, `fail_run`, `finish_stage` | Run checked skills and flows | skillrun, flow |
| `land_session` | Land session-workspace work under the default `@` | orchestrator |
| `workers`, `worker_start`, `worker_stop` | List, start and stop headless workers | cmd/alux |
| `vault_search`, `vault_read` | Search and read the markdown vault | agent, vault |
| `vault_record`, `vault_record_read` | Write and read the record store | agent, records |
| `vault_migration` | Vault-to-record migration ledger | agent, records |
| `web_search`, `web_fetch` | Search the web; fetch a page to a cached markdown handle | agent, web |
| `browser` | Drive a headless browser | agent |
| `calendar_list_writable`, `calendar_list_events`, `calendar_create_event`, `calendar_delete_event` | macOS calendars | agent, calendar |
| `pr_queue`, `pr_detail`, `pr_diff` | Read pull requests | prs |
| `pr_create`, `pr_edit`, `pr_push` | Author pull requests | prs |
| `pr_pending_review`, `pr_pending_review_discard`, `pr_thread_reply`, `pr_review_bot_run` | Review actions | prs |
| `review_load`, `submit_findings`, `pr_findings` | Local review storage and persona findings | prs |
| `mcp__<server>__<tool>` | Every connected MCP server's tools | mcpclient |

## Shell and background processes

`shell`, `procs`, `clip`, `awake`, `wake`, `netfail`

- **Persistent shell**: one live bash per agent; `cd`, variables, functions
  and aliases carry over. It can be forked, restarted or handed off.
- **Hold instead of kill**: a foreground command past its time is held for
  the agent to background or kill.
- **Wake conditions**: a background process wakes the agent when it exits,
  at the first line matching `until`, or at each `check_in` (once or
  repeating).
- **Process-group kills**: killing a process takes its children with it, and
  every process is killed on quit.
- **Output clipping and spill**: long output keeps its head and tail and
  saves the whole to a named file; carriage-return progress redraws are
  collapsed.
- **Keep the Mac awake**: `caffeinate -i` runs while finite work is in
  flight, and lets go below 20% battery when off the charger.
- **Sleep awareness**: polls and timers count only awake time and log the
  wake.
- **Network-failure detection**: tells "unreachable" output apart from a real
  answer, so polls retry with backoff instead of stopping.

## Safety and permissions

### Modes and approvals

`session`, `agent`

| Mode | Runs without asking | Sandbox network |
| --- | --- | --- |
| read-only | Read | off |
| local | Read, Build, Edit, Record | off |
| auto (default) | the above plus Network | on |

- shift+tab cycles the mode, and the mode is re-read on every call.
- Read-only also asks before editor writes, subagent dispatches and process
  kills.
- A call a gate deliberately routes to the person is never auto-approved,
  even in auto.
- With nobody to ask (subagents, unattended runs) such a call is refused,
  not passed.
- **Capability policy**: per-agent allow-lists with Deny/Ask/Allow; deny by
  default, longest prefix wins, and a line with several commands takes the
  strictest verdict. Rule sets are validated when built, and a misconfigured
  subagent stops alux from starting.
- **Skill preapproval**: a skill run the user started can preapprove the
  tools it declares. This lapses once something sensitive has been read.
- Refusals are worded as structural, so the agent doesn't retry another way.

### Guardian command screening

`guardian`

- A local model (`d3:2b` on Ollama) scores a command against eight
  categories: read, build, edit, record, rewrite, destroy, network, system.
  The command is stopped when the weight outside the agent's allowed
  categories passes a per-agent threshold.
- **Applies to commands that leave the sandbox** (`sandbox: false`), and to
  every command when the sandbox is off. Sandboxed calls skip guardian but
  still go through policy and the shell rules below.
- **Fails closed**: if Ollama is down or the answer is malformed, the user is
  asked, or the call is refused when unattended.
- **Risk labels** low/medium/high, shown on the tool box.
- **Tool suggestion**: when view, grep or glob could do the job, the command
  is held and the tool suggested. `decline_suggestion` escalates the original
  command to the user.
- **Startup health check** of Ollama's presence, version and model.
- **Audit log** of every verdict and decision (`guardian/verdicts.jsonl`,
  mode 0600).
- **Subagent routing**: with `routing` set, guardian sizes each exploring
  subagent's task and sends small and medium ones to cheaper models.

### Sandbox

`sandbox`

- Shell commands run under macOS Seatbelt. Writes are denied except to the
  project root, temp directories, tool caches and the jj/git stores. Network
  is denied except localhost, and only the main agent in auto mode gets it.
- Read-only mode makes the main agent's sandbox read-only; subagents get
  read-only or scoped-root sandboxes, never with network.
- Always write-denied: shell rc files, `.gitconfig`, `.ssh`, alux's own
  config/share/state/cache directories, and VCS trust files (`.git/hooks`,
  git/jj config, workspace pointers).
- Paths are resolved through symlinks before reaching the profile,
  `/bin/bash` is pinned, and AppleEvents are denied.
- Output that looks like a sandbox block comes with a hint on how to ask to
  run outside. If the sandbox can't be built, the command doesn't run.

### Shell-command policy

`agent`

- **Refused**: command and process substitution, grouping, unterminated
  quotes, verbs built by expansion, and file redirection or heredocs (only
  fd duplication and `/dev/null` are allowed).
- **Refused indirection**: `eval`, `exec`, `source`, nested shells, `env`,
  `xargs`, `sudo`, `nohup`, `time`, `timeout`, `watch` and the like.
- **Refused code-running options**: `git -c`, `jj --config`, `find -exec` /
  `-delete`, and env assignments such as `GIT_SSH_COMMAND`, `PAGER`,
  `LD_PRELOAD`, `DYLD_*`.
- **Fenced** `cd`, `rm` (no recursive forms) and `chmod` (one plain mode),
  all confined to the agent's edit fence.
- **GitHub**: posting to a PR from the shell is always refused, pointing at
  the `pr_*` tools. Other `gh` writes go to the user. A fixed set of `gh`
  reads may run inside `poll`.
- **Held with a suggestion**: shell reads and searches (`cat`, `head`,
  `sed -n`, `grep`, `find`…) point at view/grep/glob; sleep loops point at
  `poll`/`run_background`.
- Touching the session registry or worker channel directory goes to the
  user.

### File guards

`agent`

- No file tool may read `~/.ssh`, the record store directory, the auth file
  or MCP tokens.
- No file tool may write the sandbox's protected paths. alux `settings.json`
  and skills need an explicit yes.
- **Pool-slot guard**: a pool session may not write the default workspace,
  and edits in its own slot are refused until its jj change has a
  description.
- Symlinks are resolved (dangling links too) and writes never go through a
  link.

### Sensitivity, taint and web egress

`taint`, `web`, `agent`

- A one-way sensitivity latch is set when a phi record or entity (phi marks
  sensitive personal data, such as health or patient details), or a vault
  note with any sensitivity marking, is read.
- Once set, `web_search` and `web_fetch` refuse every call, auto drops to
  local, skill preapproval stops covering bash and network, and the browser
  reaches only localhost. The top bar shows "sensitive read · web off ·
  local".
- `web_fetch` only fetches URLs from an earlier search result or the user.
- The fetcher refuses private and non-routable addresses, checking the
  resolved IP on every redirect hop.
- Record writes must declare a sensitivity.

### Secret scrubbing

`scrub`

- Redacts known token prefixes (GitHub, Anthropic, OpenAI, Slack, AWS,
  Google, GitLab, npm, Linear, JWT), PEM keys, URL credentials, Bearer
  tokens, secret-looking `key=value` pairs, and the values of alux's own
  secret environment variables.
- Leaves placeholders, paths and code references such as `process.env.X`
  alone.

### Standing rules

`agent`, `rulecel`, `records`

- Rules are records carrying a trigger (repo, tool, bash prefix, path glob,
  agent, topic, or always) and a mode:
  - **advise** shows the rule once, after the first call it fires on;
  - **hold** stops the call until the rule has been read;
  - **hold with a CEL `when`** holds only while the condition is true.
- CEL variables are `call.*` and `session.*`. Functions: `words`, `cmd`,
  `hasFlag`, `positional`, `broadRoot`, `revs`, `descendants`, `listed`.
  Conditions are type-checked when written; a runtime error fails open and is
  reported once.
- Rules with only a topic go in a topic index for `read_rule`.
- **Provenance nonce**: a secret per-session nonce marks every rule,
  skill-notice and rule-index block. Any unmarked block, or one inside a tool
  result, is treated as data.
- Holds run before the approval prompt, so the user is never asked about a
  call the agent might withdraw.

### Ask-user

`ask`

- Up to 6 questions with 8 options each, branching on earlier answers. Each
  skipped question is reported with a reason.
- Questions always reach the person, whatever the mode.
- The agent's reply is scanned for questions aimed at the user, which are
  turned into `ask_user` input.

### Advisory

`classify`, `sidecar/`

- An optional local write-classifier sidecar marks bash commands it reads as
  writing to disk with ✎. It gates nothing.

## Context and compaction

`compact`, `session`, `agent`

- **Model-free compaction**: builds a compacted history from the transcript
  with no model call. Older sentences that newer text already covers are
  dropped, judged by local embeddings.
- **Numbered steps** (`@42`, `[120-135]`) that `recall` expands later, up to
  40k characters per call.
- **`/compact`** starts a new session seeded with the compacted history.
- **`/context`** measures the next prompt part by part with count_tokens, as
  bars, with tools grouped by source.
- **Context gauge** in the status line, with a baseline counted before the
  first reply.

## Sessions

`session`

- **Prompt queue**: prompts typed mid-turn are queued (up to 32); they can be
  pulled back or dropped.
- **Steering**: a queued prompt can be sent into the running turn at its next
  step.
- **Two-stage cancel**: the first esc aborts cleanly, the second forces it.
- **New session** (`/clear`) without restarting; background runs keep going.
- **Background delivery**: finished processes and subagent reports arrive as
  wakes on the queue.
- **Cheap-model helpers**: a recap when the pane regains focus, one-sentence
  command explanations, and automatic session naming.
- **Edited-files diff**: a per-file diff from before the session first
  touched each file.
- **Handoff**: `/handoff` writes a note and starts a new session with it, or
  copies it to the clipboard.
- **Prompt audit**: every approval and question is logged with its outcome.

## Subagents

`subagent`, `orchestrator`

- **Dispatch** with `task`: the brief has objective, done, report, scope,
  boundaries, context and seeds. Several dispatches in one reply run at once.
- **Detached by default**: reports arrive mid-turn or start a new turn;
  `background: false` waits instead.
- **Run control**: `agent_status`, `agent_cancel` (which writes a closing
  report) and check-ins on quiet runs.
- **Run budget**: `agent_extend` grants more turns; every sixth grant goes
  to the user.
- **Questions back**: a detached run can block on `ask_main`, answered with
  `agent_reply`.
- **Seeds**: files the main agent viewed are re-read fresh into the
  subagent's context.
- **Model routing** by task size (`routing` setting).
- **Dispatch log and per-run transcripts**.

| Agent | Powers |
| --- | --- |
| research | Read-only: view, glob, grep, look-only shell, vault reads; answers in prose |
| coder | Edits and builds in its own jj workspace; no network, commit or push; file locks per edit |
| pair | Coder plus a navigator that plans, reviews at checkpoints and answers `ask_navigator` |
| verifier | Research tools plus the browser; checks a UI change on a running app, with screenshots and video |
| review | Reviews one PR locally through an optional persona; cannot post |
| walkthrough | Chat about one review finding (finding chat) |
| curator | Scheduled `record-cleanup` |
| migrator | One vault note into the record store per run; no web tools |
| research-digest | Weekly harness-engineering digest |

- **Coder workspaces**: each coder starts at `@-` or a freshly fetched
  `main`, or edits an existing change in place.
- **`land_run`**: squashes a coder's change into `@` and reports any
  conflict. `drop` abandons the change, or restores one edited in place.
- **Pinned skill**: a dispatched run can carry a skill, whose result and
  waivers come back with its landing.

## Skills and flows

`skills`, `skillrun`, `flow` (see `docs/skill-flow.md`,
`docs/skill-call-predicates.md`)

- **Loader**: built-in YAML skills, replaced by a same-named file in
  `~/.config/alux/skills/`. A file with `nodes:` is a checklist; one with
  `stages:` is a flow.
- **Headers**: `when`, `not_for`, `starts: [user|model]`, `requires`, inputs
  and vars, with `${input}`/`${VAR}`/`${now_utc}` expansion (shell-quoted
  inside bash).
- **Evidence-checked steps**: each step is matched against alux's own call
  log, files on disk and the web cache, never the agent's claims. Evidence
  kinds: `call` (with CEL predicates), `fetches_opened`, `artifact_lines`,
  `citations_fetched`, `leads_followed`, `subagent_skill`, `poll_stopped`.
- **Progress notices**: a nonce-marked `<skill-notice>` names steps done out
  of order.
- **Waivers**: `waive: [{step, reason}]` closes a step without evidence and
  is recorded; `strict` steps cannot be waived.
- **Flows**: the stage cursor is driven by `finish_stage`, with `out:`
  JSON-schema outputs and a per-stage evidence window. Stage kinds:
  - `checklist`: the model works it;
  - `run`: alux makes one call and captures output by regex;
  - `spawn`: a pinned subagent;
  - `foreach`: per item, optionally parallel;
  - `repeat`: with `max`/`until`.
- **Flow control**: `when`/`until` conditions and `on_fail` loops back to an
  earlier stage. A stage that asks the user is a gate: nothing after it
  starts until it finishes.
- **`/skills`**: list, run, show as a graph, or end.

### Built-in skills

| Skill | Purpose |
| --- | --- |
| `dev-cycle` | A ticket or goal from research through plan, coders and reviews to watched draft PRs |
| `coder-task` | One planned change with a verify command (work → verify, retrying up to 3 times) |
| `change-review` | Review one jj change against a written spec |
| `pr-review` | Local-only PR review by a persona panel, with triage and an optional e2e check |
| `pr-ship` | Verify, push, open a draft PR, run a review bot such as Bugbot (when configured per repo) and watch hand-written code |
| `spec` | Write a technical spec, then critique it with a blind persona panel (`spec-critic`) |
| `e2e-verify` | Check a running web app in a browser per scenario, with recordings |
| `deep-research` | Multi-hop web research with cited, fetched sources |
| `research-digest` | Weekly digest on agent harnesses and agentic coding |
| `record-cleanup` | Work down record conflicts, unfiled entries, look-alike pairs; gist sessions |
| `smoke`, `smoke-spawn`, `smoke-child` | Cheapest end-to-end checks of skill runs and spawn stages |

The larger skills are flows built from sub-skills (`dev-*`, `pr-review-*`,
`pr-ship-*`, `spec-*`, `e2e-*`).

## Workers and workspaces

`workers`, `orchestrator`

- **Headless workers**: `worker_start` (needs the user's yes) runs a served
  alux in its own pool workspace, keyed by ticket or `WORKER-N`, starting
  from `main` or `@-`. `max_workers` caps them (default 1).
- **Worker ledger**: states running, waiting, stalled, idle, crashed, done
  and failed, with audit and prompt logs. Exactly one TUI claims each final
  report.
- **Worker messages**: `/workers send` delivers a line to a live worker.
  Parked worker prompts show up in other panes, tagged `[worker]`.
- **Workspace pool**: each session gets a prepared jj workspace, with a spare
  readied ahead of time. A slot starts at the newest described ancestor of
  the default `@`.
- **Prepare**: copies ignored state copy-on-write from the main checkout,
  rewrites paths, runs setup commands, and refreshes when lockfiles change.
  `coders: true` prepares coder workspaces too.
- **Flows `land` and `pr`**: in `pr` flow nothing lands in the default
  workspace; work ships from trunk.
- **Session landing**: `/land` or `land_session` puts a slot's change and its
  unlanded ancestors under the default `@`.
- **`/resume <change>`** carries on from an ended session's change.

## jj integration

`vcs`, `orchestrator`

- **Status line VCS**: file and line counts scoped to the slot's own work,
  plus the PR number of its bookmark. It reads without snapshotting the
  working copy.
- **Ship state per change**: on trunk, pushed, bookmark conflict, ahead or
  behind trunk, moved by another push.
- **From the workspaces pane**: push every bookmark down to trunk; fetch and
  rebase a stack onto trunk, undoing the move if it would create a new
  conflict.
- **Sweep**: abandons pool work whose PR merged, only when the change is
  exactly the head GitHub merged.
- **Race-free diffs**: change diffs are read without snapshotting, so they
  cannot race an agent's own jj commands.

## Pull requests

`prs`

- **PR queue**: repos and query rules come from settings (`review`/`mine`
  lists with exclusions and flags). Polled every 4 minutes, it keeps the last
  good queue on error and shares one cache across every alux on the machine.
- **Reading**:
  - `pr_detail`: verdicts, failing checks, threads, files, the PR environment
    and your drafted comments;
  - `pr_diff`: a saved diff with a per-file index and incremental `since`
    diffs.
- **Pending reviews only**: `pr_pending_review` drafts line-anchored,
  labelled comments and never submits; discarding needs approval.
- **Thread replies**: `pr_thread_reply` always asks, except an un-mentioning
  reply on a thread opened by a review bot such as Bugbot, when configured
  per repo.
- **Review bot**: `pr_review_bot_run` asks a review bot such as Bugbot, when
  configured per repo, to run; unasked on drafts, asked otherwise.
- **Authoring**:
  - `pr_create` opens drafts only, with image and video uploads;
  - `pr_edit` changes title, body, ready state and labels, asking before
    ready or the ready/merge label;
  - `pr_push` refuses the default branch and merged PRs.
- **Merge label**: rechecks that the PR is yours, approved, mergeable and not
  failing before labelling it.
- **Review apps**: reads the deploy label, bot and health check, and picks
  the app to test from the changed paths.
- **Stacks**: finds PRs above and below by head and base; merged PRs are
  detected for the sweep.

## Code review

`reviewers`, `reviewstore`, `reviewgraph`, `prs`

- **Persona panel**: 13 personas, two always on and the rest chosen by
  changed path (at most 4): test skeptic, bug finder, database, auth,
  query performance, API contract, distributed systems, integration,
  frontend, privacy, security, operability, maintainer.
- **`submit_findings`**: each persona's findings are anchor-checked against
  the diff at the PR head. `pr_findings` renders them.
- **Triage**: merges, groups or discards findings, with a verdict each.
- **`review_load`**: one stored review per PR, pinned to its head commit.
  Dismissals and per-finding threads carry across re-reviews.
- **Review graph**: `sem diff` names the changed entities, and a ripgrep pass
  finds the files that mention them. Drawn as the PR file graph.
- **Finding chat**: ask an agent about one finding inside the PR's review
  workspace; the question and answer are stored on the finding.

## Record store

`records` (`~/.local/share/alux/records.db`)

- **SQLite, append-only**: triggers forbid updates and deletes. A change is a
  superseding record. Twelve embedded migrations run under the write lock.
- **Nine kinds** with typed attributes: claim, state, decision, measurement,
  todo, question, procedure, rule, argument.
- **Entities**: person, system, ticket, project, repo, term, team, org and
  topic, with aliases and one-hop merges.
- **Provenance**: each write records actor, agent, model and session; each
  record has a typed source.
- **Derived status**: active, superseded, done, expired, not_yet, retracted
  or dropped.
- **Write ops**: add, supersede (one or many), link (7 roles), cite, slot,
  event, entity and entity_edit, unfile and resolve, judge, episode.
- **Read ops**: search (FTS5, then partial, link-graph and semantic matches),
  get, about, episode, conflicts, pairs, unfiled, ungisted.
- **Time travel**: `as_of` (what held in the world) and `known_at` (what the
  store knew).
- **Duplicate detection**: on write, similar records and entities are
  suggested by token overlap and cosine similarity. Suggestions and verdicts
  are logged.
- **Embeddings**: `embeddinggemma` on local Ollama, filled in the background
  (`ALUX_RECORDS_EMBED=0` turns them off).
- **phi sensitivity**: phi marks sensitive personal data, such as health or
  patient details. Records about phi entities must be phi. phi is never
  paired and is counted, not named, in backlog listings.
- `ALUX_RECORDS=0` turns the store off; any other value is a path to use.

## Vault

`vault`

- **Read access** to a configured markdown folder. Paths outside it,
  `.obsidian` and symlinks are refused.
- **`vault_search`**: a section-level FTS index (headings 5×, aliases 2.5×),
  refreshed incrementally, with a ripgrep fallback.
- **`vault_read`**: a whole note or one section, paged. It opens with a
  notice when the note has been superseded.
- **Link graph**: wikilinks and backlinks by role, plus unresolved links.
- **Note supersession**: declared in frontmatter (authoritative), or
  detected heuristically for audit.
- **Lexical aliases**: LLM paraphrases per section, resumable, generated by
  `/vault aliases`.
- **`/vault`**: index status and rebuild.
- **Narrow internal write surface** (create, append, exact replace) behind a
  machine-wide lock.

## Vault-to-record migration

`records`, `agent`, `subagent`

- **Ledger**: per-note pending, in_progress, done, skipped or failed.
  Seeding is idempotent and reports notes that vanished.
- **Claims**: token-guarded, with a 1-hour expiry.
- **`vault_migration` tool**: seed, claim, finish, status.
- **Auto-migrate** (`auto_migrate: true`): reading a pending note starts a
  background migrator. A done note's records are appended to the read.
- **Migrator subagent**, with no web tools.

## Background jobs

`jobs`, `subagent`

- **Scheduler**: a `jobs` block in settings (agent, interval ≥ 10m,
  enabled). With no block, the curator alone runs every 3h.
- **One run machine-wide**, only when there is a backlog, locked on the job
  stamp.
- **Run log** at `~/.local/state/alux/jobs/runs.jsonl`; `/jobs` shows each
  job's last outcome and next due time.
- **Curator**: record cleanup and session gisting.
- **Research digest**: a weekly cited digest under the jobs output directory.

## Workflow mining

`mine` (see `docs/workflow-miner.md`)

- **Capture**: per-turn tool-call symbol sequences, with no arguments or
  output, in `~/.local/state/alux/mine/sequences.jsonl`.
- **Bash encoder**: splits command lines, drops read-only verbs, and keeps
  subcommands (`jj_commit`).
- **Miner**: runs in the background at startup and on `/new`. PrefixSpan
  with support, lift and significance scoring, and pattern families.
- **Output**: `candidates.md` lists skill candidates, script candidates and
  families already covered by a skill. `dismissed.txt` hides a candidate.

## MCP

`mcpclient`

- Claude-Code-style `mcpServers`: user servers, then project `.mcp.json`
  servers (opt-in via `enabledMcpjsonServers`), then local per-project ones.
- Stdio (spawned command) and remote (`url`) servers.
- OAuth with dynamic client registration and browser sign-in; tokens persist,
  refresh safely across processes, and re-register when stale.
- `mcp_tools` allow-lists each server's tools; `mcp_defer` loads a server's
  tools only through tool search.
- `coder_mcp_readonly` names the MCP tool prefixes coders may call; alux
  warns when a rule admits a tool the server says is not read-only.
- Image results are supported.

## Web

`web`

- **Search providers**: SearXNG or Brave, sharing one query shape (`count`,
  `site`, `since`). Rate limits and exhausted plans are reported distinctly.
- **`web_fetch`**: returns a handle (cache path, title, word count, heading
  outline) instead of the page. The cache is shared machine-wide; `refresh`
  re-fetches.
- **Fetch safety**: SSRF protection, URL provenance checks, and closure after
  a sensitive read (see [Safety](#sensitivity-taint-and-web-egress)).

## Browser

`agent` (needs `browser.binary`, an `agent-browser` path)

- **Actions**: open, snapshot, read, click, fill, upload, press, scroll,
  mouse, wait, requests, console, errors, screenshot, record_start,
  record_stop, close.
- **Snapshots**: page-changing actions return a snapshot with element refs.
- **Recording**: MP4 under /tmp, with idle stretches cut and a visible
  gliding cursor. `record_stop` returns a 3×3 frame sheet.
- **Host allowlist**: localhost always. Other hosts come from `browser.allow`
  (exact hosts or `*.domain`) or a saved allow-once. Subagents are refused
  unlisted hosts.
- **After a sensitive read**: page-input actions need approval and only
  localhost is reachable.

## Calendar

`calendar` (macOS, `"calendar": true`)

- Native EventKit reads and writes; recurring events resolve to the
  occurrence in the window.
- Event notes are never returned, and `pwd=` meeting passcodes are scrubbed.
- Creating an event shows a preview for approval. Delete is meant for undoing
  events this session created and refuses recurring series.

## Terminal UI

`ui`, `markdown`, `graphview`, `code`, `diff`

### Screens

- **Main screen**: a top bar, the transcript, a "running" sidebar and the
  input frame. Alt screen, with mouse and focus reporting.
- **Welcome screen**: wordmark, greeting, mode, key hints, a random tip, and
  a setup checklist (guardian, rg/fd, API key, records, vault, PRs, calendar)
  with fix-it hints.
- **Running sidebar**: NEEDS YOU, AGENTS, TO LAND, COMMANDS, SKILL,
  BACKGROUND, SESSIONS and FILES. It collapses to a glyph rail when narrow;
  agents get hashed hats and colours, and a pair is bracketed.
- **Expanded panel** overlay, hosting:
  - the subagent run view;
  - the skill graph view;
  - the file diff view;
  - the workspaces pane;
  - the PR file graph.
- **PR picker** (`/prs`): stacked PR rows with status icons, author, idle
  time, a reviewed mark and a legend.
- **PR file graph**: changed files with counts and finding marks, the hovered
  file's neighbourhood graph, and findings boxed under their patch lines.
- **Workspaces pane** (`/land`): slot changes as cards with PR, remote,
  conflict, size and CI chips; a graph mode; and per-change diffs.
- **Graph layout**: Graphviz dot (WASM) with orthogonal routing, falling back
  to a sketch layout.

### Status line

- Set into the input frame's border. In order:
  - "working…" or "cancelling…";
  - the keep-awake glyph or a low-battery warning;
  - jj status with the slot's PR number;
  - the mode, coloured;
  - a context key hint;
  - the worker name and stage.
- The context gauge (yellow at 70%, red at 90%) and cache hit rate sit on
  the right; the session topic is centred.
- The frame colour shows state: idle, working, turn just ended, approval,
  question, picker.
- Top-bar PR summary: flags, review and mine counts, and "stale" when a poll
  failed.

### Transcript rendering

- Markdown through glamour, with width-wrapped tables.
- Mermaid fences are drawn as text diagrams, falling back to the source.
- Streaming replies are rendered live. PR numbers, ticket keys and record ids
  are resolved, underlined and clickable.
- Collapsible tool boxes with a coloured bar per tool kind; `compose` steps
  are bracketed.
- Box badges: ✎ for a write, an unsandboxed marker, and the guardian verdict.
- Box bodies:
  - edits as syntax-highlighted diffs;
  - views as numbered listings;
  - searches as per-file results;
  - poll logs in aligned columns.
- Background commands show a live tail; thinking shows while in flight.
- A pinned prompt header when scrolled back, and an "↓ N lines below" pill.
- An animated mascot with moods (think, work, wait, web, delegate, need,
  done, doze) that speaks in-app notices.

### Input

- A multi-line editor that grows to a third of the pane, with bracketed
  paste.
- Slash-command completion above the frame.
- The message queue shows above the input; clicking a queued message steers
  it in now.
- A recap after more than 3 idle minutes, when focus returns.

### Prompts

- Approval prompts show highlighted commands or edit diffs, with `y`/`n`.
- The question panel has a ❯ cursor, per-option detail, "Other…" free text,
  per-option notes and "N of M" progress.
- Headless worker prompts are answerable from any pane.

### Keybindings

**Main**

| Key | Action |
| --- | --- |
| `enter` / `shift+enter` | Submit / newline |
| `tab` | Accept slash completion |
| `esc` | Clear selection, set aside a worker prompt, or cancel the turn (again to force) |
| `backspace` (empty) | Pull back the last queued message |
| `shift+tab` | Cycle mode |
| `y` / `n` | Approve / deny |
| `pgup` / `pgdown`, `up` / `down` | Scroll |
| `ctrl+[` / `ctrl+]` (or `alt+↑/↓`) | Previous / next prompt |
| `end` | Follow the tail |
| `⌘s` / `⌘e` | Toggle the sidebar / focus it |
| `ctrl+c` / `ctrl+d` | Kill the session (confirms when agents are running) |

**Sidebar**: `↑↓`/`jk` move, `enter` open, `x x` stop a session, `esc`
back.

**Panels**: `esc`/`q` close, `jk` scroll, `space`/`pgdn` page, `g`/`G` top
and bottom.

**Question panel**: `↑↓`/`jk` choose, `→`/`l` add a note, `enter` answer.

**PR picker**

| Key | Action |
| --- | --- |
| `enter` | Details |
| `r` | Review |
| `l` | Ask about the PR |
| `g` | File graph |
| `e` | e2e check against the review app |
| `m` | Merge check and label |
| `o` | Open in the browser |
| `y` | Confirm a label |

**File graph**

| Key | Action |
| --- | --- |
| `→`/`l` | Into the findings |
| `enter` | Diff |
| `[` `]` | Previous / next finding |
| `c` | Ask about a finding |
| `x` | Dismiss a finding |
| `d` | Draft the review on GitHub |

**Workspaces**

| Key | Action |
| --- | --- |
| `enter` | Land |
| `s` | Resume |
| `d` | Diff |
| `t` | Graph mode |
| `r` | Refresh |
| `p` | Push (pr flow) |
| `u` | Fetch and rebase (pr flow) |
| `o` | Draft a PR (pr flow) |

**Mouse**:

- click to expand a box or open a run;
- drag to select, copied over OSC 52;
- click an ID to show its name, or a queued message to steer it;
- click the PR summary to open the picker;
- the wheel scrolls.

## Slash commands

`commands`

| Command | What it does |
| --- | --- |
| `/clear` (`/new`) | New session |
| `/spawn` | New session in its own pool workspace |
| `/kill` | Stop this session and switch to another |
| `/quit` | Stop this session and close the pane |
| `/detach` | Close the pane, leave the session running |
| `/handoff [copy]` | Write a handoff note and continue in a new session, or copy it |
| `/compact` | New session seeded with this one's compacted history |
| `/context` (`/ctx`) | Where the prompt's tokens go |
| `/vault [rebuild\|aliases]` | Vault index status, rebuild, or generate aliases |
| `/prs [N\|refresh]` | PR queue, one PR, or fetch now |
| `/workers [KEY\|send KEY text]` | Worker table, one worker, or message a worker |
| `/land [change]` | Workspaces pane, or land a change |
| `/resume <change>` | Continue from an ended session's change |
| `/skills [run\|show\|end]` | List, start, draw, or close a skill run |
| `/agents` | This session's subagent runs |
| `/jobs` | Scheduled jobs and their last runs |

Commands typed mid-turn wait in the prompt queue.

## Notifications

`ui`, `desktop`, `programstatus`, `registry`

- **In-app notices**: a mascot speech bubble or a corner card, lasting 6s,
  up to 4 queued.
- **Desktop notifications** through `osascript`, sent only when the terminal
  isn't frontmost. A batch of finishing subagents notifies once.
- **OSC 7501 program status** (idle, working, done, blocked) for terminal
  multiplexers.
- **Session registry**: each session publishes its state for other panes'
  sidebars. The registry is read-only: no session can drive another.

## Developer tools

| Path | Purpose |
| --- | --- |
| `internal/mockapi` (`mockserve`) | A scripted stand-in for the Anthropic Messages API via `ANTHROPIC_BASE_URL` |
| `bin/setup` | Install dependencies, pull models, link `alux` |
| `bin/alux` | Rebuild the binary on source change, then run it |

## Extras

- **`extras/claude-code/demo-video`**: a Claude Code skill that records a
  web-app demo with agent-browser and ffmpeg. It draws a gliding pointer,
  cuts pauses, and outputs a cut MP4, a full MP4 and a frame sheet.
