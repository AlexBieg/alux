#!/usr/bin/env python3

import argparse
import json
import os
import signal
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

EMBEDDED_QUESTIONS = {
    "writes": {
        "type": "noul",
        "instructions": "This is a shell command an AI coding agent wants to run. Does running it create, modify, overwrite, append to, or delete any file on disk?",
        "criteria": {
            "true": "the command writes, changes or deletes a file on disk",
            "false": "the command only reads, inspects, searches or prints",
        },
    }
}


# The shape below must match the state the model was fine-tuned on, key order and truncations included.
def enrich(cmd):
    toks = cmd.replace("|", " ").replace(";", " ").split()
    return {
        "command": cmd,
        "program": toks[0] if toks else "",
        "has_output_redirect": any(t in (">", ">>") for t in toks),
        "has_heredoc": "<<" in cmd,
        "pipes_into_tee": "tee" in cmd,
        "flags": [t for t in toks if t.startswith("-")][:6],
        "paths": [t for t in toks if ("/" in t or "." in t) and not t.startswith("-")][:6],
    }


def log(msg):
    print(msg, file=sys.stderr, flush=True)


# Preferred over the embedded copy because it is byte-identical to the question the model trained on.
def load_questions(model_path):
    path = os.path.join(model_path, "questions.json")
    if not os.path.exists(path):
        log(f"no {path}, using embedded question definition")
        return {"writes": EMBEDDED_QUESTIONS["writes"]}
    with open(path) as f:
        schema = json.load(f)
    if "writes" not in schema:
        log(f"{path} has no 'writes' question, using embedded question definition")
        return {"writes": EMBEDDED_QUESTIONS["writes"]}
    return {"writes": schema["writes"]}


class Handler(BaseHTTPRequestHandler):
    agent = None
    questions = None
    lock = None
    model_path = ""
    device = ""

    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        log(f"{self.address_string()} {fmt % args}")

    def _reply(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.split("?")[0] != "/health":
            self._reply(404, {"error": "not found"})
            return
        self._reply(200, {"ok": True, "model": self.model_path, "device": self.device})

    def do_POST(self):
        if self.path.split("?")[0] != "/classify":
            self._reply(404, {"error": "not found"})
            return

        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            self._reply(400, {"error": "bad Content-Length"})
            return
        raw = self.rfile.read(length) if length > 0 else b""

        try:
            payload = json.loads(raw)
        except (ValueError, UnicodeDecodeError) as e:
            self._reply(400, {"error": f"malformed JSON: {e}"})
            return
        if not isinstance(payload, dict):
            self._reply(400, {"error": "body must be a JSON object"})
            return
        cmd = payload.get("command")
        if not isinstance(cmd, str):
            self._reply(400, {"error": "'command' must be a string"})
            return

        state = enrich(cmd)
        start = time.perf_counter()
        try:
            # torch on MPS is not safe to call concurrently, so predictions are serialised.
            with self.lock:
                result = self.agent.predict(state, self.questions)
            writes = float(result["answers"]["writes"]["noul"])
        except Exception as e:
            log(f"inference failed: {e!r}")
            self._reply(500, {"error": f"inference failed: {e}"})
            return
        self._reply(200, {"writes": writes, "ms": (time.perf_counter() - start) * 1000.0})


def main():
    ap = argparse.ArgumentParser(description="HTTP sidecar serving a fine-tuned Laya classifier")
    ap.add_argument("--model", required=True, help="path to the checkpoint directory")
    ap.add_argument("--port", type=int, default=8899)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--device", default="mps", choices=("mps", "cpu"))
    args = ap.parse_args()

    # laya.load exists only in the PyPI wheel; this checkpoint needs the git checkout's entry point.
    from laya.agent import load as laya_load

    log(f"loading {args.model} on {args.device}")
    agent = laya_load(args.model, device=args.device)
    questions = load_questions(args.model)

    Handler.agent = agent
    Handler.questions = questions
    Handler.lock = threading.Lock()
    Handler.model_path = args.model
    Handler.device = args.device

    server = ThreadingHTTPServer((args.host, args.port), Handler)

    def stop(signum, frame):
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop)

    print(f"READY port={server.server_address[1]}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
