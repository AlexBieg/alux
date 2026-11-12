# alux

alux (ah-LOOSH) is a terminal coding agent for macOS, built on Claude. It
is one person's harness, shaped around one way of working, and it changes
often. It is more useful as a source of ideas than as a tool to adopt as is.

The name comes from Yucatec Maya folklore. An alux is a small spirit a
farmer invites onto his land. It works hard inside the walls built for it,
and runs wild outside them.

One rule shapes the design: the model acts only where deterministic code
cannot, and the harness guards everything it does.

## What's in it

- **Safety in layers.** Every shell command is parsed against a per-agent
  allow-list first. Anything the parser cannot read with certainty, such
  as `eval` or command substitution, is refused. What passes runs in a
  Seatbelt sandbox set by the mode. A command that asks to leave the
  sandbox goes to the guardian, a small local model that scores what the
  command does. The harness, not the model, decides whether it runs or
  asks you.
- **Undo through jj.** There are no checkpoints. alux snapshots the repo
  with jj whenever the agent goes idle, so `jj op log` and `jj undo` reach
  any earlier state, whatever changed the files.
- **Skills checked against evidence.** A skill is a flow of steps. A step
  is done only when the harness's own log holds the tool call that proves
  it, matched by a CEL condition. The model can waive a step, but only by
  saying why, on the record.
- **A record store instead of notes.** The agent keeps typed records about
  named entities: one fact each, append-only, each with its source. Rules
  live there too. Each rule reaches the agent at the first tool call it
  applies to, rather than sitting in the prompt all session.
- **A workspace per session.** Each session gets its own jj workspace from
  a pool, and each coder subagent gets another. Work waits as a described
  change until you land it.
- **Compaction in code.** `/compact` folds the transcript into a ledger
  without asking a model to summarise it. Each ledger line keeps the step
  numbers it replaced, and `recall` brings those steps back in full.
- **Local PR review.** A panel of reviewer subagents, picked from the
  changed paths, reviews a pull request. Findings are drawn beside the
  code. Nothing reaches GitHub except as a pending review you submit
  yourself.
- **Browser checks with recordings.** A verifier subagent drives a
  headless browser through accessibility snapshots, and records a video
  with the idle stretches cut.

[FEATURES.md](FEATURES.md) lists everything in detail.

## Install

With [Homebrew](https://brew.sh) installed:

```sh
git clone <this repo> ~/Code/alux
~/Code/alux/bin/setup
export ANTHROPIC_API_KEY=...   # or put it in ~/.config/alux/auth.json
alux
```

`bin/setup` installs whatever is missing of mise (for Go), ripgrep, fd,
jj, gh and ffmpeg, and links `alux` into `~/.local/bin`. It is safe to
re-run.

`bin/setup --ollama` also installs and starts Ollama and pulls the `d3:2b`
and `embeddinggemma` models, which the guardian and record search use.

`bin/alux` rebuilds the binary whenever the source changes. The first
launch compiles it, which takes a minute.

With `--ollama`, the two models take about 3.7 GB of disk. `d3:2b` needs about 8 GB of
RAM, and both stay loaded for 30 minutes after their last use, so an 8 GB
Mac will feel it.

### API key

alux needs an Anthropic API key. A Claude subscription login does not
work. Either export `ANTHROPIC_API_KEY`, or write
`~/.config/alux/auth.json`:

```json
{ "anthropic": { "type": "api_key", "key": "sk-ant-..." } }
```

then `chmod 600 ~/.config/alux/auth.json`.

## Modes

shift+tab cycles the mode, shown in the status line. alux starts in auto.

| Mode | Asks before | Sandbox network |
| --- | --- | --- |
| read-only | edits, subagents, killing processes, and any command beyond reading | off |
| local | commands beyond reading, building and editing | off |
| auto | commands beyond reading, building, editing and network | on |

The guardian sorts every shell command into one of those kinds; one
outside the mode's kinds, or one it cannot judge, asks you first.

## What happens without Ollama

The guardian cannot judge anything, so every shell command stops and asks
you, and subagents running unattended have theirs refused. alux warns at
launch when Ollama is missing, too old, or lacks the model. Record search
falls back to keyword and link matches.

## Configuration

Everything is optional. Settings live in `~/.config/alux/settings.json`.
`settings.example.json` shows the common keys. Its paths and repo names
are placeholders: copy only the keys you need, and fill them in.

| Key | What it does |
| --- | --- |
| `guardian` | Ollama URL and model for screening commands |
| `web` | Search provider for `web_search` (`brave` with a key, or `searxng` with a `url`) |
| `vault` | A folder of markdown notes the agent can search |
| `workspaces` | Per-repo jj workspace pools for parallel sessions |
| `prs` | Repos whose pull requests alux tracks |
| `coder_mcp_readonly` | MCP tool prefixes the coder subagent may call |
| `mcpServers` | MCP servers to connect to |
| `sandbox` | `{"enabled": false}` turns the sandbox off |
| `browser` | `{"binary": "/abs/path/to/agent-browser"}` enables the browser tool |
| `calendar` | `true` lets the agent read and edit your macOS calendars; macOS asks for permission on first use |
| `routing` | `{"small": "<model>", "medium": "<model>"}` has guardian size each exploring subagent's task and runs small and medium work on those models; large work keeps the agent's own |
| `auto_migrate` | `true` migrates each vault note into the record store on first read |
| `tickets_dir` | A vault folder holding one `<KEY> <title>` folder per ticket; a session on a branch naming a ticket is pointed at its folder |
| `tickets` | `{"mcp_server": "<name>"}`: the `mcpServers` entry whose read-only issue tool names ticket keys in the transcript |
| `knowledge_tool` | The MCP tool, such as `mcp__wiki__search`, that the research and intake skills search for internal context; their step for it is skipped when unset |

Per-repo PR keys:

- `review_app`: `{label, bot, host_suffix, health_check}`, for CI that
  deploys a preview environment per PR
  - `heading`: text the bot's comment must contain; unset, any comment by `bot` with app lines counts
  - `slug_app`: the app whose host is read first for the environment's slug
  - `apps_dir`: the path prefix, such as `apps/`, under which each folder is an app; e2e testing picks the app the changed files touch
  - `skip`: app folders under `apps_dir` never picked
  - `rank`: apps in tie-break order; the first is tested when no changed file names an app
  - `login_app`: the app e2e tests log in through
- `ready_label`: a label that may only go on a PR once it leaves draft
- `bugbot`: `{"trigger": "<comment>", "login": "<bot login>"}` enables a
  review-bot tool that posts the trigger comment; `true` means Cursor
  Bugbot (`"bugbot run"`, `cursor`)

Skills ship built in. A file of the same name in `~/.config/alux/skills/`
replaces the built-in.

## Records

alux keeps a knowledge store at `~/.local/share/alux/records.db`: typed
facts, decisions and todos the agent writes as it learns them and reads
back in later sessions. It is on by default, and the agent tells you in one
line when it stores something. `ALUX_RECORDS=0` turns it off; any other
value is a path to use instead.

A record or note marked sensitive closes web access for the rest of the
session, and auto mode drops to local; the status line then says so.

## Notifications

alux sends desktop notifications through `osascript`, so macOS delivers
them as Script Editor. Allow Script Editor in System Settings →
Notifications, or you will not see them.

## License

Copyright (C) 2026 Alex Bieg. alux is free software under the GNU General
Public License, version 3; see [LICENSE](LICENSE).
