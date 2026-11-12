# classify sidecar

`classify_server.py` serves one fine-tuned Laya classifier over HTTP: given a shell command, it
answers how likely that command is to write to disk. It loads the checkpoint once at startup and
serialises inference behind a lock, so it is meant to be run as a long-lived sidecar next to
alux rather than spawned per request.

The checkpoint is **not** in this repository. Train or fetch it separately and point `--model` at
the directory. If that directory holds a `questions.json`, the `writes` question is read from it so
the served prompt is byte-identical to the trained one; otherwise an embedded copy is used.

## Setup

```sh
python3 -m venv .venv
.venv/bin/pip install torch
.venv/bin/pip install -e /path/to/laya        # git checkout, not the wheel
```

Install `laya` from a git clone: `laya.train` and the 0.4.0 API are not in the PyPI wheel, and the
wheel's `laya.load` is not the entry point this checkpoint needs.

## Run

```sh
.venv/bin/python sidecar/classify_server.py --model /path/to/checkpoint --port 8899 --host 127.0.0.1 --device mps
```

`--port` defaults to 8899, `--host` to 127.0.0.1, `--device` to `mps` (`cpu` also works). The
server prints `READY port=8899` on stdout once the model is loaded and the port is bound; every
other message goes to stderr. SIGTERM shuts it down cleanly.

## Endpoints

```sh
curl -s http://127.0.0.1:8899/health
# {"ok": true, "model": "/path/to/checkpoint", "device": "mps"}

curl -s -X POST http://127.0.0.1:8899/classify \
  -H 'Content-Type: application/json' \
  -d '{"command": "rm -rf build/"}'
# {"writes": 0.98, "ms": 42.1}
```

A malformed body or a missing/non-string `command` gives HTTP 400, and a failed prediction gives
HTTP 500, both as `{"error": "..."}`.

## Turning it on in alux

The feature is off until `~/.config/alux/settings.json` configures it, and alux starts
and stops the sidecar itself:

```json
{
  "classify": {
    "python": "/path/to/.venv/bin/python",
    "model": "/path/to/checkpoint",
    "script": "sidecar/classify_server.py",
    "port": 8899,
    "device": "mps"
  }
}
```

`python` and `model` are required; the rest take the defaults above. `script` should be absolute:
a relative path resolves against whatever directory alux was started in. A bash command the
model reads as a write gets a `✎` beside its marker in the transcript; everything else is left
alone.

## How much to trust it

Measured on a held-out split of the corpus it was trained on: 0.992 AUROC, 99% accuracy, ~35 ms
per command. On a hand-written probe of tools absent from that corpus it scores 0.855 AUROC and
86% accuracy, and its mistakes there are confident ones. Treat the tint as a prompt to look, not
as an answer — it is advisory, and nothing is gated on it.
