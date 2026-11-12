---
name: demo-video
description: Record a demo video of a web app with agent-browser, showing a visible pointer that glides to each target and rings each click, with idle stretches cut, saved as MP4. Use when asked for a demo video, screen recording or walkthrough of a UI.
allowed-tools: Bash(agent-browser:*), Bash(~/.claude/skills/demo-video/*), Read
---

# Demo video

Needs `agent-browser` and `ffmpeg` (with `ffprobe`) on PATH. The scripts are in this skill's folder, `~/.claude/skills/demo-video/`.

## Steps

1. Plan the walkthrough first: the URL, and each state the viewer should see, in order.
2. Start recording: `start.sh /tmp/demo.mp4 <url>`. It sets a 1280×800 viewport (override with `DEMO_WIDTH`/`DEMO_HEIGHT`) and starts the recording.
3. For each step:
   - `agent-browser snapshot -i` to get element refs.
   - **Always** run `aim.sh @eN` immediately before `agent-browser click @eN` or `agent-browser fill @eN "text"`. Without it the video shows things changing with no pointer, and the viewer cannot tell what was clicked.
   - After a state the viewer needs to read, hold it with `agent-browser wait 2500`. Pauses of 2 s or longer are cut to 1 s, so a hold over 1 s buys nothing beyond reading time, but a hold under 1 s is too short to read.
4. Stop: `finish.sh /tmp/demo.mp4`. It saves the MP4 with still stretches cut, an uncut `-full.mp4`, and a `-frames.png` sheet of 9 frames tiled 3×3 in order.
5. Read the `-frames.png` sheet to check the video shows the intended flow. If a step is missing or wrong, record again from step 2.
6. Report the MP4 path to the user. Run `agent-browser close` when done.

## Notes

- Use the same agent-browser session throughout (set `AGENT_BROWSER_SESSION` or `--session` consistently); the scripts use the default session.
- Do not use `agent-browser record stop` directly: it leaves a raw WebM with no cuts.
