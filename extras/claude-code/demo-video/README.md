# demo-video, a Claude Code skill

Has Claude record a web app demo as an MP4: a drawn pointer glides to each target and rings each click, and idle stretches are cut.

Install:

```sh
brew install ffmpeg && npm install -g agent-browser && agent-browser install
cp -r demo-video ~/.claude/skills/
```

Then ask Claude Code for "a demo video of <flow> at <url>". It also needs `python3`, which macOS ships.
