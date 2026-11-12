# alux

A terminal coding agent for macOS, built on Claude. It runs shell commands
in a Seatbelt sandbox and screens each one with a small local model before
it runs.

macOS only.

## Install

With [Homebrew](https://brew.sh) installed:

```sh
git clone <this repo> ~/Code/alux
~/Code/alux/bin/setup
export ANTHROPIC_API_KEY=...   # or put it in ~/.config/alux/auth.json
alux
```

`bin/setup` installs whatever is missing of mise (for Go), ripgrep, fd,
Ollama, jj, gh and ffmpeg, starts Ollama, pulls the `d3:2b` and
`embeddinggemma` models, and links `alux` into `~/.local/bin`. It is safe
to re-run.

`bin/alux` rebuilds the binary whenever the source changes. The first
launch compiles it, which takes a minute.

The two models take about 3.7 GB of disk. `d3:2b` needs about 8 GB of
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
launch when Ollama is missing, too old, or lacks the model.

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
