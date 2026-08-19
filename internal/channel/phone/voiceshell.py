#!/usr/bin/env python3
"""Factor's voice shell.

This is the only Patter-facing surface in the whole phone channel. It is
written to disk by Factor on start and run in a private virtualenv; everything
it needs arrives as one JSON blob in FACTOR_VOICE_CONFIG, so no secret is ever
visible in argv.

What it does:

  * builds a Patter pipeline agent (speech-to-text -> Factor -> text-to-speech)
    whose "LLM" is Factor itself, reached over loopback through the bridge's
    OpenAI-compatible endpoint;
  * enforces the inbound allowlist and the maximum call length by hanging the
    call up at the carrier;
  * exposes a tiny control API (GET /health, POST /call) for Factor;
  * forwards call-started and call-ended events back to the bridge, so a call
    Factor placed can report its outcome into the conversation that asked for
    it.

Everything Patter-specific is confined to build_stt/build_tts/build_llm and
main(); the rest is standard library.
"""

from __future__ import annotations

import asyncio
import base64
import importlib
import inspect
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# ---------------------------------------------------------------- configuration


def load_config() -> dict:
    raw = os.environ.get("FACTOR_VOICE_CONFIG")
    if not raw:
        die("FACTOR_VOICE_CONFIG is not set; the voice shell is started by Factor, not by hand")
    try:
        return json.loads(raw)
    except ValueError as exc:
        die(f"FACTOR_VOICE_CONFIG is not valid JSON: {exc}")


def log(message: str, **fields: object) -> None:
    extra = " ".join(f"{k}={v}" for k, v in fields.items())
    line = f"[voiceshell] {message}"
    if extra:
        line += f" {extra}"
    print(line, file=sys.stderr, flush=True)


def die(message: str) -> None:
    log("fatal: " + message)
    raise SystemExit(2)


CFG = load_config()

BRIDGE_URL = CFG["bridge_url"].rstrip("/")
BRIDGE_TOKEN = CFG["bridge_token"]
CARRIER = CFG["carrier"]
ALLOW_FROM = CFG.get("allow_from") or []
ALLOW_CALL_TO = CFG.get("allow_call_to") or []
MAX_CALL_SECONDS = int(CFG.get("max_call_seconds") or 900)
RING_TIMEOUT = 25

# ------------------------------------------------------------ bridge callbacks


def post_json(url: str, payload: dict, timeout: float = 10.0) -> dict:
    body = json.dumps(payload).encode()
    request = urllib.request.Request(url, data=body, method="POST")
    request.add_header("Content-Type", "application/json")
    request.add_header("Authorization", "Bearer " + BRIDGE_TOKEN)
    with urllib.request.urlopen(request, timeout=timeout) as response:
        raw = response.read().decode() or "{}"
    try:
        return json.loads(raw)
    except ValueError:
        return {}


def notify(payload: dict) -> None:
    """Tell Factor about a call lifecycle change. Never raises: a bridge that
    is briefly unreachable must not tear down a live call."""
    try:
        post_json(BRIDGE_URL + "/internal/call-event", payload)
    except Exception as exc:  # noqa: BLE001 - best effort by design
        log("could not reach the bridge", event=payload.get("event"), error=repr(exc))


# ------------------------------------------------------------- carrier control


TELNYX_API = "https://api.telnyx.com"


def telnyx_request(path: str, payload: dict | None = None, method: str = "POST"):
    """Build an authenticated Telnyx REST request. A payload of None sends no
    body, which is what the reads want."""
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request(TELNYX_API + path, data=data, method=method)
    request.add_header("Authorization", "Bearer " + CARRIER.get("api_key", ""))
    if data is not None:
        request.add_header("Content-Type", "application/json")
    return request


def twilio_hangup_request(call_sid: str):
    account = urllib.parse.quote(CARRIER["account_sid"], safe="")
    url = f"https://api.twilio.com/2010-04-01/Accounts/{account}/Calls/{urllib.parse.quote(call_sid, safe='')}.json"
    request = urllib.request.Request(
        url, data=urllib.parse.urlencode({"Status": "completed"}).encode(), method="POST")
    credentials = f"{CARRIER['account_sid']}:{CARRIER['auth_token']}".encode()
    request.add_header("Authorization", "Basic " + base64.b64encode(credentials).decode())
    request.add_header("Content-Type", "application/x-www-form-urlencoded")
    return request


def carrier_hangup(call_sid: str) -> None:
    """End a call at the carrier.

    Used for callers who are not allowed to reach the agent and for calls that
    have run past max_call_minutes — Patter exposes no hook for either, so the
    carrier's own API is the lever. call_sid is Patter's call id, which is the
    Call SID on Twilio and the call control id on Telnyx; a mismatch is logged
    rather than raised, since neither guardrail is worth dropping a live call
    over."""
    if not call_sid:
        return
    name = CARRIER.get("name")
    if name == "twilio":
        request = twilio_hangup_request(call_sid)
    elif name == "telnyx":
        request = telnyx_request(
            f"/v2/calls/{urllib.parse.quote(call_sid, safe='')}/actions/hangup", {})
    else:
        log("cannot hang up: this shell does not speak the configured carrier", carrier=name)
        return
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            response.read()
    except Exception as exc:  # noqa: BLE001
        log("carrier hangup failed", call=call_sid, error=repr(exc))


def bind_telnyx_number() -> None:
    """Attach the number to the Call Control Application.

    Patter does this for Twilio on every start and for Telnyx never, so Factor
    does it: a number that answers to no application rings nowhere."""
    number = urllib.parse.quote(CARRIER.get("phone_number", ""), safe="")
    try:
        with urllib.request.urlopen(
            telnyx_request(f"/v2/phone_numbers/{number}/voice",
                           {"connection_id": CARRIER.get("connection_id", "")}, method="PATCH"),
            timeout=10,
        ) as response:
            response.read()
    except Exception as exc:  # noqa: BLE001
        # Not fatal: a number already assigned in the portal works fine, and
        # this is the one call that fails when it is assigned to something else
        # on purpose.
        log("could not attach the number to the telnyx application", error=repr(exc))


def point_telnyx_webhook(url: str) -> None:
    """Point the Call Control Application at this shell.

    Telnyx keeps the webhook URL on the application rather than on the number,
    and Patter only auto-configures Twilio — so nothing else would ever tell
    the carrier where a freshly-tunnelled shell is answering."""
    app = urllib.parse.quote(CARRIER.get("connection_id", ""), safe="")
    path = f"/v2/call_control_applications/{app}"
    try:
        with urllib.request.urlopen(telnyx_request(path, method="GET"), timeout=10) as response:
            current = json.loads(response.read() or b"{}").get("data") or {}
    except Exception as exc:  # noqa: BLE001
        log("could not read the telnyx call control application", error=repr(exc))
        return
    if current.get("webhook_event_url") == url:
        return
    try:
        with urllib.request.urlopen(
            # Telnyx requires the name on every update, so it is read back and
            # sent unchanged rather than invented here.
            telnyx_request(path, {
                "application_name": current.get("application_name") or "Factor",
                "webhook_event_url": url,
            }, method="PATCH"),
            timeout=10,
        ) as response:
            response.read()
    except Exception as exc:  # noqa: BLE001
        log("could not point the telnyx webhook at this shell", url=url, error=repr(exc))
        return
    log("telnyx webhook set", url=url)


async def register_telnyx_webhook(phone) -> None:
    """Wait for the public hostname, then hand it to the carrier.

    tunnel_ready resolves as soon as the hostname is known — immediately for a
    static webhook_url, once cloudflared answers for a quick tunnel — which is
    exactly when the carrier can be told where to knock."""
    ready = getattr(phone, "tunnel_ready", None)
    if ready is None:
        log("this patter release exposes no tunnel_ready; set the telnyx webhook by hand")
        return
    try:
        hostname = await ready
    except Exception as exc:  # noqa: BLE001
        log("the tunnel never came up; leaving the telnyx webhook alone", error=repr(exc))
        return
    host = str(hostname).split("://", 1)[-1].strip("/")
    await asyncio.to_thread(bind_telnyx_number)
    await asyncio.to_thread(point_telnyx_webhook, f"https://{host}/webhooks/telnyx/voice")


def caller_allowed(number: str) -> bool:
    return "*" in ALLOW_FROM or number in ALLOW_FROM


def callee_allowed(number: str) -> bool:
    return number in ALLOW_CALL_TO


# ------------------------------------------------------------- provider wiring


def build(factory, **kwargs):
    """Construct a provider, passing only the keyword arguments it accepts.

    Patter is young and its adapters gain and lose keywords between releases;
    dropping an unknown one costs a default, while passing it would take the
    whole channel down."""
    try:
        return factory(**kwargs)
    except TypeError:
        try:
            accepted = set(inspect.signature(factory).parameters)
        except (TypeError, ValueError):
            raise
        pruned = {k: v for k, v in kwargs.items() if k in accepted}
        dropped = sorted(set(kwargs) - set(pruned))
        if dropped:
            log("provider does not accept some settings; using its defaults",
                provider=getattr(factory, "__qualname__", str(factory)), dropped=",".join(dropped))
        return factory(**pruned)


# Attribute names Patter's REST adapters might keep their endpoint under. The
# documented constructors take no base_url, so pointing Whisper/OpenAI-TTS at a
# local server means reaching for the attribute — done here, loudly, rather
# than by forking the SDK.
_BASE_URL_ATTRS = ("base_url", "_base_url", "api_base", "_api_base", "endpoint", "_endpoint")

# Where each adapter keeps its endpoint when it keeps it nowhere else: as of
# getpatter 0.6.2 both REST speech adapters post to a module-level constant and
# hold no endpoint on the instance at all, so the last resort is to rebind the
# constant. The provider reads it out of module globals on every request, which
# is what makes this work rather than merely typecheck.
# The only transcription model name Patter's adapter accepts without argument.
# It names the protocol, not the weights.
_OPENAI_STT_MODEL = "whisper-1"

_URL_CONSTANTS = {
    "stt": ("getpatter.providers.whisper_stt", "OPENAI_TRANSCRIPTION_URL", "/audio/transcriptions"),
    "tts": ("getpatter.providers.openai_tts", "OPENAI_TTS_URL", "/audio/speech"),
}


def build_local(factory, base_url: str, what: str, **kwargs):
    """Construct a speech provider aimed at a local OpenAI-compatible server.

    Three strategies, weakest binding last: a constructor that accepts base_url
    (which a future Patter release may well), an endpoint attribute on the
    instance, and finally the module constant the adapter posts to."""
    try:
        return factory(base_url=base_url, **kwargs)
    except TypeError:
        pass
    provider = build(factory, **kwargs)
    for attr in _BASE_URL_ATTRS:
        if hasattr(provider, attr):
            setattr(provider, attr, base_url)
            log("local audio server wired", stage=what, base_url=base_url, via=attr)
            return provider

    module_name, constant, path = _URL_CONSTANTS[what]
    try:
        module = importlib.import_module(module_name)
    except ImportError:
        module = None
    if module is not None and hasattr(module, constant):
        setattr(module, constant, base_url + path)
        log("local audio server wired", stage=what, base_url=base_url, via=f"{module_name}.{constant}")
        return provider

    die(
        f"this Patter release exposes no endpoint override on its {what} adapter, so the local "
        f"tier cannot be wired ({base_url}). Point channels.phone.{what}.provider at a cloud "
        f"provider, or keep local_audio_fallback on so Factor degrades instead of failing calls."
    )


def build_stt():
    stt = CFG["stt"]
    provider = stt.get("provider")
    if provider == "deepgram":
        from getpatter.stt import deepgram

        return build(deepgram.STT, api_key=stt.get("api_key"), model=stt.get("model"),
                     language=stt.get("language"))
    if provider in ("whisper", "local-openai"):
        from getpatter.stt import whisper

        kwargs = {
            "api_key": stt.get("api_key") or "local",
            "model": stt.get("model") or None,
            "language": stt.get("language"),
        }
        if provider == "local-openai":
            # Patter's adapter validates the model against OpenAI's own names
            # and raises — not a TypeError, so build() would not catch it — for
            # anything else, including None. A local server's model ids are its
            # own (faster-whisper sizes, Hugging Face repos), so construct with
            # a name the adapter accepts and put the real one back afterwards:
            # it is read off the instance on every request.
            wanted = kwargs.pop("model")
            adapter = build_local(whisper.STT, stt["base_url"], "stt",
                                  model=_OPENAI_STT_MODEL, **kwargs)
            if wanted:
                adapter.model = wanted
            return adapter
        return build(whisper.STT, **kwargs)
    die(f"unknown stt provider {provider!r}")


def build_tts():
    tts = CFG["tts"]
    provider = tts.get("provider")
    if provider == "elevenlabs":
        from getpatter import ElevenLabsWebSocketTTS

        # Each factory emits what its carrier negotiates — mu-law at 8 kHz for
        # Twilio, linear PCM at 16 kHz for Telnyx. Matching it is what lets
        # Patter skip the transcode, worth roughly 50 ms per utterance.
        wanted = "for_telnyx" if CARRIER.get("name") == "telnyx" else "for_twilio"
        factory = getattr(ElevenLabsWebSocketTTS, wanted, ElevenLabsWebSocketTTS)
        return build(factory, api_key=tts.get("api_key"), voice_id=tts.get("voice") or None,
                     model_id=tts.get("model") or None)
    if provider == "local-openai":
        from getpatter.tts import openai as openai_tts

        # The rate stays at Patter's 16 kHz default: both carriers' senders
        # resample 16 kHz down to the phone band themselves, and handing them
        # 8 kHz instead is read as 16 kHz — audio at half speed, an octave low.
        return build_local(
            openai_tts.TTS, tts["base_url"], "tts",
            api_key=tts.get("api_key") or "local",
            model=tts.get("model") or None,
            voice=tts.get("voice") or None,
        )
    die(f"unknown tts provider {provider!r}")


def build_llm():
    """Factor itself, reached over loopback.

    This release's OpenAI adapter stamps no per-request header, so a turn
    arrives at the bridge untagged and is matched to the call that is up — the
    single-live-call path the bridge already keeps for exactly this. The
    spoken filler for a long turn needs no header either: the bridge sends it
    as an early content delta on the turn's own stream, so it reaches the
    caller through the same adapter, ahead of the answer."""
    from getpatter import OpenAILLM

    # Patter's OpenAI provider builds its own client and takes no endpoint, so
    # the bridge is bound onto the client afterwards. This is not optional
    # politeness: an unbound client points at api.openai.com, which would send
    # the caller's words to a third party under a key that is not one.
    llm = build(OpenAILLM, api_key=BRIDGE_TOKEN, model="factor")
    client = getattr(llm, "_client", None) or getattr(llm, "client", None)
    if client is None or not hasattr(client, "base_url"):
        die(
            "this Patter release exposes no endpoint override on its OpenAI LLM adapter, so "
            "the agent cannot be reached over loopback. The phone channel cannot run safely "
            "against it — pin the release Factor was built for."
        )
    client.base_url = BRIDGE_URL + "/v1"
    if not str(client.base_url).startswith(BRIDGE_URL):
        die(f"the LLM adapter kept {client.base_url} instead of the bridge at {BRIDGE_URL}")
    log("bridge wired", base_url=str(client.base_url))
    return llm


# ------------------------------------------------------------------ call state


class Calls:
    """Correlates Patter's call ids with the ids Factor was handed.

    phone.call() returns nothing, so an outbound call gets a local id up front
    and is matched to Patter's id when it connects."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.pending: dict[str, dict] = {}  # local id -> {"to", "at"}
        self.live: dict[str, dict] = {}     # patter id -> {"local", "machine", "timer"}

    def expect(self, local_id: str, to: str) -> None:
        with self._lock:
            self.pending[local_id] = {"to": to, "at": time.time()}

    def adopt(self, patter_id: str, callee: str, direction: str) -> str:
        """Bind a connecting call to its local id (outbound) or to itself."""
        with self._lock:
            local_id = patter_id
            if direction == "outbound":
                for candidate, info in list(self.pending.items()):
                    if info["to"] == callee:
                        local_id = candidate
                        del self.pending[candidate]
                        break
            self.live[patter_id] = {"local": local_id, "machine": False, "timer": None}
            return local_id

    def get(self, patter_id: str) -> dict | None:
        with self._lock:
            return self.live.get(patter_id)

    def mark_machine(self, local_id: str) -> None:
        with self._lock:
            for info in self.live.values():
                if info.get("local") == local_id:
                    info["machine"] = True

    def drop(self, patter_id: str) -> dict | None:
        with self._lock:
            return self.live.pop(patter_id, None)

    def give_up(self, local_id: str) -> dict | None:
        """Forget an outbound call that never connected."""
        with self._lock:
            return self.pending.pop(local_id, None)


CALLS = Calls()


def transcript_tail(entries, limit: int = 20) -> str:
    lines = []
    for entry in (entries or [])[-limit:]:
        if isinstance(entry, dict):
            role = entry.get("role", "?")
            text = entry.get("text") or entry.get("content") or ""
        else:
            role = getattr(entry, "role", "?")
            text = getattr(entry, "text", "")
        if text:
            lines.append(f"{role}: {text}")
    return "\n".join(lines)


# ----------------------------------------------------------------- call hooks


async def on_call_start(event: dict) -> None:
    call_id = str(event.get("call_id") or "")
    caller = str(event.get("caller") or "")
    callee = str(event.get("callee") or "")
    direction = str(event.get("direction") or "inbound")

    if direction == "inbound" and not caller_allowed(caller):
        log("rejecting a caller who is not on the allowlist", caller=caller, call=call_id)
        await asyncio.to_thread(carrier_hangup, call_id)
        return

    local_id = CALLS.adopt(call_id, callee, direction)
    await asyncio.to_thread(notify, {
        "event": "call_started",
        "call_id": local_id,
        "session_id": call_id,
        "from": caller,
        "to": callee,
        "direction": direction,
    })

    live = CALLS.get(call_id)
    if live is not None:
        live["timer"] = spawn(enforce_max_duration(call_id))


async def enforce_max_duration(call_id: str) -> None:
    """Hang up a call that has run past its budget. A phone line left open by a
    confused conversation is a bill, so the limit is enforced here rather than
    trusted to the model."""
    try:
        await asyncio.sleep(MAX_CALL_SECONDS)
    except asyncio.CancelledError:
        return
    log("call exceeded max_call_minutes; hanging up", call=call_id, seconds=MAX_CALL_SECONDS)
    await asyncio.to_thread(carrier_hangup, call_id)


async def on_call_end(event: dict) -> None:
    call_id = str(event.get("call_id") or "")
    live = CALLS.drop(call_id) or {}
    timer = live.get("timer")
    if timer is not None:
        timer.cancel()

    transcript = transcript_tail(event.get("transcript"))
    if live.get("machine"):
        status = "voicemail"
    elif not transcript:
        status = "no-answer"
    else:
        status = "completed"

    await asyncio.to_thread(notify, {
        "event": "call_ended",
        "call_id": live.get("local", call_id),
        "session_id": call_id,
        "status": status,
        "transcript": transcript,
    })


def machine_detection_handler(local_id: str):
    """Remember that a call was answered by a machine, so its outcome reads
    "voicemail" rather than "completed"."""
    def handle(result) -> None:
        outcome = getattr(result, "outcome", None) or getattr(result, "result", None) or str(result)
        if "machine" in str(outcome).lower():
            CALLS.mark_machine(local_id)
    return handle


# ---------------------------------------------------------------- control API


class ControlHandler(BaseHTTPRequestHandler):
    """Factor's side door: is the shell alive, and please dial this number."""

    server_version = "FactorVoiceShell/1"
    loop: asyncio.AbstractEventLoop
    phone = None
    agent = None

    def log_message(self, fmt: str, *args: object) -> None:  # quieter than the default
        return

    def _reply(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802 - http.server's naming
        if self.path.rstrip("/") in ("/health", ""):
            self._reply(200, {"status": "ok", "tier": CFG.get("tier"), "carrier": CARRIER.get("name")})
            return
        self._reply(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path.rstrip("/") != "/call":
            self._reply(404, {"error": "not found"})
            return
        # Dialling costs money, so it takes the boot secret even on loopback.
        if self.headers.get("Authorization", "") != "Bearer " + BRIDGE_TOKEN:
            self._reply(401, {"error": "unauthorized"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        try:
            payload = json.loads(self.rfile.read(length) or b"{}")
        except ValueError as exc:
            self._reply(400, {"error": f"invalid json: {exc}"})
            return

        to = str(payload.get("to") or "")
        if not callee_allowed(to):
            self._reply(403, {"error": f"{to} is not on the outbound allowlist"})
            return

        local_id = uuid.uuid4().hex
        CALLS.expect(local_id, to)
        future = asyncio.run_coroutine_threadsafe(
            place_call(local_id, to, str(payload.get("goal") or ""), str(payload.get("first_message") or "")),
            self.loop,
        )
        try:
            future.result(timeout=30)
        except Exception as exc:  # noqa: BLE001 - reported to Factor verbatim
            CALLS.give_up(local_id)
            self._reply(502, {"error": f"the carrier refused the call: {exc}"})
            return
        self._reply(200, {"call_id": local_id})


# Tasks are kept referenced so the event loop cannot garbage-collect a call
# that is still ringing.
BACKGROUND: set[asyncio.Task] = set()


def spawn(coro) -> asyncio.Task:
    task = asyncio.create_task(coro)
    BACKGROUND.add(task)
    task.add_done_callback(BACKGROUND.discard)
    return task


async def place_call(local_id: str, to: str, goal: str, first_message: str) -> None:
    """Start dialling and return.

    phone.call() may return as soon as the carrier accepts the request or stay
    open for the whole conversation, so this waits only long enough to catch an
    outright refusal and then lets it run."""
    task = spawn(build_call(ControlHandler.phone, ControlHandler.agent, to, first_message, local_id))
    done, _ = await asyncio.wait({task}, timeout=5)
    if task in done:
        task.result()  # re-raises a carrier refusal to the control API
    spawn(watch_for_answer(local_id, to))
    log("dialling", to=to, call=local_id, goal=goal[:80])


async def build_call(phone, agent, to: str, first_message: str, local_id: str) -> None:
    kwargs = {
        "to": to,
        "agent": agent,
        "first_message": first_message,
        "machine_detection": bool(CFG.get("machine_detection", True)),
        "voicemail_message": CFG.get("voicemail_message") or "",
        "ring_timeout": RING_TIMEOUT,
        "on_machine_detection": machine_detection_handler(local_id),
    }
    try:
        await phone.call(**kwargs)
    except TypeError:
        accepted = set(inspect.signature(phone.call).parameters)
        await phone.call(**{k: v for k, v in kwargs.items() if k in accepted})


async def watch_for_answer(local_id: str, to: str) -> None:
    """Report a call that never connected.

    Patter fires no end event for a call that was never answered, and an agent
    that is never told how its call went is worse than one that cannot call at
    all."""
    await asyncio.sleep(RING_TIMEOUT + 15)
    if CALLS.give_up(local_id) is None:
        return  # it connected; on_call_end owns it now
    log("outbound call was never answered", to=to, call=local_id)
    await asyncio.to_thread(notify, {
        "event": "call_ended",
        "call_id": local_id,
        "status": "no-answer",
        "transcript": "",
    })


def serve_control(loop: asyncio.AbstractEventLoop, phone, agent) -> ThreadingHTTPServer:
    ControlHandler.loop = loop
    ControlHandler.phone = phone
    ControlHandler.agent = agent
    server = ThreadingHTTPServer((CFG.get("control_host", "127.0.0.1"), int(CFG["control_port"])), ControlHandler)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, name="factor-control", daemon=True).start()
    log("control api listening", port=CFG["control_port"])
    return server


# ----------------------------------------------------------------------- main

SYSTEM_PROMPT = (
    "You are on a phone call. Your replies are produced by the Factor agent behind this "
    "endpoint, which keeps its own instructions, memory, and tools — say nothing here."
)


def build_carrier():
    """The carrier Patter dials through. Factor's config layer refuses anything
    other than these two long before the shell starts."""
    name = CARRIER.get("name")
    if name == "twilio":
        from getpatter import Twilio

        return build(Twilio, account_sid=CARRIER["account_sid"], auth_token=CARRIER["auth_token"])
    if name == "telnyx":
        from getpatter import Telnyx

        return build(Telnyx, api_key=CARRIER["api_key"], connection_id=CARRIER["connection_id"],
                     public_key=CARRIER.get("public_key", ""))
    die(f"carrier {name!r} is not wired in this shell")


async def main() -> None:
    from getpatter import Patter

    carrier = build_carrier()
    quick_tunnel = CFG.get("tunnel") == "quick"
    phone = build(
        Patter,
        carrier=carrier,
        phone_number=CARRIER["phone_number"],
        webhook_url="" if quick_tunnel else CFG.get("webhook_url", ""),
        tunnel=True if quick_tunnel else None,
        persist=False,
    )

    agent = build(
        phone.agent,
        system_prompt=SYSTEM_PROMPT,
        stt=build_stt(),
        llm=build_llm(),
        tts=build_tts(),
        language=CFG.get("language", "en"),
        first_message=CFG.get("first_message", ""),
        barge_in_threshold_ms=int(CFG.get("barge_in_threshold_ms", 300)),
    )

    serve_control(asyncio.get_running_loop(), phone, agent)
    if CARRIER.get("name") == "telnyx":
        # Started before serve(), which does not return: it waits on the same
        # hostname serve() is about to publish.
        spawn(register_telnyx_webhook(phone))
    log("starting patter", tier=CFG.get("tier"), tunnel=CFG.get("tunnel"), port=CFG.get("webhook_port"))
    await build(
        phone.serve,
        agent=agent,
        port=int(CFG.get("webhook_port") or 8000),
        on_call_start=on_call_start,
        on_call_end=on_call_end,
        voicemail_message=CFG.get("voicemail_message") or "",
        # The dashboard fails open without a token, and this process holds
        # carrier credentials: it stays off.
        dashboard=False,
        tunnel=quick_tunnel,
    )


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass
