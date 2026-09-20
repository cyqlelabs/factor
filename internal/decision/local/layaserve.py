#!/usr/bin/env python3
"""Factor's local decision server.

Laya is a decision model rather than a chat model: it reads a state and
answers typed questions about it in one forward pass of an encoder, with no
text generated and so nothing to parse and nothing to hallucinate. It ships
as a Python library, not a server, so Factor brings the server — written to
disk on start and run in a private virtualenv, exactly like the voice shell
and the speech server, with its configuration arriving as one JSON blob in
FACTOR_DECISION_CONFIG.

The route is the one TypeSafe's hosted model answers, POST /v1/systemone,
because Laya's own reply is already that response — answers keyed by question
id, each with a choice, a probability per candidate, a confidence, and a token
count. Speaking the same wire format on both sides means Factor has one
decision client and the backend is a matter of which process answers, not of
which protocol it speaks. Any other local server of that contract — Kev,
jev-local, LitJev — drops into the same seam with no code at all.

Three things here are load-bearing.

**The model is loaded before the server answers anything.** Laya keeps one
checkpoint resident and rebuilds on a switch, which its own README measures at
a 7.4 second median on CPU. Factor's decisions run under a short deadline
while a user waits, so a rebuild mid-conversation is not a slow decision but a
missed one. Loading happens once, at start, and the health probe says no until
it is done.

**The multilingual checkpoint is the one it loads.** Laya ships an English
checkpoint that scores higher on English, and it is not offered here. On
anything else it does not merely score worse — it collapses while staying
confident (its authors measure Khmer at 0.000 accuracy and 0.952 confidence),
which is the one failure shape a confidence gate cannot catch. Factor answers
in whatever language its user speaks and reads states — pages, tool results,
a person's own words — in that language. The multilingual checkpoint is also
the smaller of the two (322M against 421M) and carries twice the context, so
the safe choice is the cheap one and there is nothing here to choose.

**Limits are reported rather than assumed.** A question's options all have to
fit in the head's token budget, and the state is silently truncated to what is
left of the window. Both numbers depend on the checkpoint that actually
loaded, so /health states them and Factor sizes its requests against what it
is told — a browser page offering sixty controls has to know it may send
twelve before it sends sixty and gets a refusal, or worse, a guess.
"""

from __future__ import annotations

import json
import os
import sys
import threading
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Hugging Face's hub phones home as it resolves a repo, and a personal agent's
# sidecar should not. Factor sets this when it spawns the server; the default
# here is for a run started any other way.
os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")

MAX_BODY = 4 << 20  # a decision request is kilobytes; past this it is not one


def log(message: str, **fields: object) -> None:
    extra = " ".join(f"{k}={v}" for k, v in fields.items())
    line = f"[layaserve] {message}"
    if extra:
        line += f" {extra}"
    print(line, file=sys.stderr, flush=True)


def die(message: str) -> None:
    log(message)
    raise SystemExit(1)


def load_config() -> dict:
    raw = os.environ.get("FACTOR_DECISION_CONFIG", "").strip()
    if not raw:
        return {}
    try:
        cfg = json.loads(raw)
    except json.JSONDecodeError as exc:
        die(f"FACTOR_DECISION_CONFIG is not valid JSON: {exc}")
    if not isinstance(cfg, dict):
        die("FACTOR_DECISION_CONFIG must be a JSON object")
    return cfg


CONFIG = load_config()
HOST = CONFIG.get("host") or "127.0.0.1"
PORT = int(CONFIG.get("port") or 8731)
DEVICE = (CONFIG.get("device") or "").strip() or None

# The one checkpoint, and not a setting: see the module docstring.
REPO = "convaiinnovations/laya"
SUBFOLDER = "multilingual"

STATE = {
    "ready": False,
    "error": "",
    "max_len": 0,
    "head_max_len": 0,
}

# One forward pass at a time. The model is a GIL-bound tensor op and the boxes
# Factor runs on have a couple of cores: answering two requests at once costs
# the same wall clock as answering them in turn and risks running the machine
# out of memory mid-decision.
PREDICT_LOCK = threading.Lock()

AGENT = None


def load_model() -> None:
    """Build the checkpoint before the server answers anything.

    Laya keeps one checkpoint resident and rebuilds on a switch, which its own
    README measures at a 7.4 second median on CPU. Factor's decisions run
    under a short deadline while a user waits, so loading is done here, once,
    rather than on the first request that needs it.
    """
    global AGENT
    import laya

    kwargs = {"subfolder": SUBFOLDER}
    if DEVICE:
        kwargs["device"] = DEVICE
    AGENT = laya.load(REPO, **kwargs)
    cfg = getattr(AGENT, "cfg", {}) or {}
    STATE["max_len"] = int(cfg.get("max_len", 1024))
    STATE["head_max_len"] = int(cfg.get("head_max_len", 192))
    log("ready", max_len=STATE["max_len"], head=STATE["head_max_len"])


def answer(state, questions) -> dict:
    """One decision, as /v1/systemone returns it.

    Laya's own reply already carries answers, probabilities, confidence and a
    token count under exactly these names, so this adds a model name and gets
    out of the way.
    """
    with PREDICT_LOCK:
        result = AGENT.system_one(state, questions)
    out = dict(result)
    out["model"] = "laya-multilingual"
    return out


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args) -> None:  # the access log is noise in a sidecar
        pass

    def reply(self, code: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if self.path.split("?")[0] != "/health":
            self.reply(404, {"error": "no such route"})
            return
        # The limits ride the health probe because they are what the caller
        # has to size its requests against, and only the loaded checkpoint
        # knows them.
        self.reply(200 if STATE["ready"] else 503, {
            "ok": STATE["ready"],
            "error": STATE["error"],
            "max_len": STATE["max_len"],
            "head_max_len": STATE["head_max_len"],
        })

    def do_POST(self) -> None:
        if self.path.split("?")[0] != "/v1/systemone":
            self.reply(404, {"error": "no such route"})
            return
        if not STATE["ready"]:
            self.reply(503, {"error": STATE["error"] or "still loading the model"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0 or length > MAX_BODY:
            self.reply(400, {"error": "a decision request needs a body, and a bounded one"})
            return
        try:
            payload = json.loads(self.rfile.read(length))
        except (json.JSONDecodeError, ValueError) as exc:
            self.reply(400, {"error": f"request is not valid JSON: {exc}"})
            return
        questions = payload.get("questions")
        if not isinstance(questions, dict) or not questions:
            self.reply(400, {"error": "a decision request carries at least one question"})
            return
        state = payload.get("state")
        if state is None:
            state = ""
        try:
            self.reply(200, answer(state, questions))
        except ValueError as exc:
            # A question whose options do not fit the head is the caller's to
            # fix — it must offer fewer or shorter ones — so it is a 400 and
            # not a 500: Factor reads a 4xx as "this request was wrong" and a
            # 5xx as "the backend is unwell", and retries only the second.
            self.reply(400, {"error": str(exc)})
        except Exception as exc:  # noqa: BLE001 - a sidecar must not die of one bad request
            log("decision failed", error=repr(exc))
            traceback.print_exc(file=sys.stderr)
            self.reply(500, {"error": repr(exc)})


def main() -> None:
    try:
        load_model()
    except Exception as exc:  # noqa: BLE001
        STATE["error"] = repr(exc)
        log("could not load the model", error=repr(exc))
        traceback.print_exc(file=sys.stderr)
        raise SystemExit(1) from exc
    STATE["ready"] = True
    server = ThreadingHTTPServer((HOST, PORT), Handler)
    server.daemon_threads = True
    log("listening", host=HOST, port=PORT)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
