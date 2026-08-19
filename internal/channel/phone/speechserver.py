#!/usr/bin/env python3
"""Factor's local speech server.

The local voice tiers need an OpenAI-compatible speech server, and none that
can be pip-installed exists: Speaches ships as a container or a git checkout,
and Piper's own HTTP server speaks its own dialect. So Factor brings its own —
written to disk on start and run in a private virtualenv, exactly like the
voice shell, with its configuration arriving as one JSON blob in
FACTOR_SPEECH_CONFIG so no secret is ever visible in argv.

It implements only the two routes Patter actually calls, to the letter of what
its adapters expect:

  * POST /v1/audio/transcriptions — multipart upload of a 16 kHz mono WAV,
    answered with {"text": ...}; verbose_json also carries per-segment
    avg_logprob, which Patter turns into a confidence score.
  * POST /v1/audio/speech — {"model", "input", "voice", "response_format"},
    answered with headerless 24 kHz mono signed 16-bit PCM. Patter hardcodes
    response_format="pcm" and resamples 24 kHz down to the 8 kHz phone band, so
    anything else arrives at the caller's ear transposed.

Speech-to-text is faster-whisper (multilingual, so every language Whisper
knows), text-to-speech is Piper (49 language families, and its wheel carries
its own espeak-ng, so no system package has to be installed for any of them).
"""

from __future__ import annotations

import asyncio
import audioop
import io
import json
import os
import sys
import threading
import wave
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import Body, FastAPI, File, Form, Header, HTTPException, UploadFile
from fastapi.responses import JSONResponse, Response

# ---------------------------------------------------------------- configuration


def log(message: str, **fields: object) -> None:
    extra = " ".join(f"{k}={v}" for k, v in fields.items())
    line = f"[speechserver] {message}"
    if extra:
        line += f" {extra}"
    print(line, file=sys.stderr, flush=True)


def die(message: str) -> None:
    log("fatal: " + message)
    raise SystemExit(2)


def load_config() -> dict:
    raw = os.environ.get("FACTOR_SPEECH_CONFIG")
    if not raw:
        die("FACTOR_SPEECH_CONFIG is not set; the speech server is started by Factor, not by hand")
    try:
        return json.loads(raw)
    except ValueError as exc:
        die(f"FACTOR_SPEECH_CONFIG is not valid JSON: {exc}")


CFG = load_config()

TOKEN = CFG.get("token") or ""
DATA_DIR = Path(CFG.get("data_dir") or ".")
LANGUAGE = (CFG.get("language") or "en").split("-")[0].lower()

WHISPER_MODEL = CFG.get("whisper_model") or "base"
WHISPER_DEVICE = CFG.get("whisper_device") or "cpu"
WHISPER_COMPUTE = CFG.get("whisper_compute") or "int8"
PIPER_VOICE = CFG.get("piper_voice") or ""

# Patter's OpenAI TTS adapter resamples 24 kHz to the phone band and nothing
# else, so this rate is a contract rather than a preference.
OUTPUT_RATE = int(CFG.get("output_sample_rate") or 24000)

# Guards against Whisper's habit of inventing speech where there is none. The
# thresholds are deliberately loose: a caller who is cut off mid-word should
# still be heard, and the cost of dropping a real half-word is far lower than
# the cost of handing the agent a sentence nobody said.
MIN_TRANSCRIBE_SECONDS = 0.2
MAX_NO_SPEECH_PROB = 0.6
MIN_AVG_LOGPROB = -1.0

# ------------------------------------------------------------------- the models

_stt = None
_tts = None
_stt_lock = threading.Lock()
_tts_lock = threading.Lock()



def mute_onnx_telemetry() -> None:
    """Stop onnxruntime posting device data to Microsoft.

    Piper runs its voices on onnxruntime, which bundles Microsoft's 1DS
    client and POSTs to mobile.events.data.microsoft.com on every model
    load: OS build, CPU model, core count, memory, network type, stable
    device and session GUIDs, the model file name, and the interpreter
    path — which carries the user's name. A speech tier chosen because it
    is local should not be introducing itself to anyone.

    This has to run before the first import that pulls onnxruntime in, which
    is faster-whisper's VAD rather than Piper: the events that carry the
    device data are queued while onnxruntime registers its execution
    providers, and a switch thrown after that only silences what has not
    been sent yet.
    """
    try:
        import onnxruntime

        onnxruntime.disable_telemetry_events()
    except Exception as exc:  # an older build without the call, or no onnxruntime at all
        log("could not disable onnxruntime telemetry", error=str(exc))

def load_models() -> None:
    """Load both engines before the server reports itself healthy.

    Factor waits on /health before it starts the voice shell, so paying the
    load here is what keeps the first call of the day from opening with a
    twenty-second silence while a model is read off disk."""
    global _stt, _tts
    mute_onnx_telemetry()

    from faster_whisper import WhisperModel

    log("loading speech-to-text", model=WHISPER_MODEL, device=WHISPER_DEVICE, compute=WHISPER_COMPUTE)
    _stt = WhisperModel(
        WHISPER_MODEL,
        device=WHISPER_DEVICE,
        compute_type=WHISPER_COMPUTE,
        download_root=str(DATA_DIR / "whisper"),
    )

    if PIPER_VOICE:
        from piper import PiperVoice

        onnx = DATA_DIR / "piper" / f"{PIPER_VOICE}.onnx"
        if not onnx.exists():
            die(f"the Piper voice {PIPER_VOICE} is not downloaded ({onnx})")
        log("loading text-to-speech", voice=PIPER_VOICE)
        _tts = PiperVoice.load(str(onnx))

    log("ready", stt=WHISPER_MODEL, tts=PIPER_VOICE or "disabled", rate=OUTPUT_RATE)


@asynccontextmanager
async def lifespan(_: FastAPI):
    await asyncio.to_thread(load_models)
    yield


app = FastAPI(title="factor-speech", docs_url=None, redoc_url=None, lifespan=lifespan)


def authorize(header: str | None) -> None:
    """Check the boot secret Factor shares with its own children.

    The scheme is required: without it a bare token would authenticate, and so
    would a header that merely happened to equal the token."""
    if not TOKEN:
        return
    if not header or not header.startswith("Bearer "):
        raise HTTPException(status_code=401, detail="unauthorized")
    if header[len("Bearer "):] != TOKEN:
        raise HTTPException(status_code=401, detail="unauthorized")


# --------------------------------------------------------------------- routes


@app.get("/health")
def health() -> JSONResponse:
    ready = _stt is not None and (_tts is not None or not PIPER_VOICE)
    return JSONResponse({"status": "ok" if ready else "loading"}, status_code=200 if ready else 503)


@app.get("/v1/models")
def models() -> dict:
    """The catalogue Factor's startup probe reads to tell that we are alive."""
    listed = [{"id": WHISPER_MODEL, "object": "model", "owned_by": "faster-whisper"}]
    if PIPER_VOICE:
        listed.append({"id": PIPER_VOICE, "object": "model", "owned_by": "piper"})
    return {"object": "list", "data": listed}


@app.post("/v1/audio/transcriptions")
def transcriptions(
    file: UploadFile = File(...),
    model: str = Form(default=""),
    language: str = Form(default=""),
    response_format: str = Form(default="json"),
    authorization: str | None = Header(default=None),
):
    """Transcribe one buffered chunk of call audio.

    Patter uploads roughly a second of 16 kHz mono WAV at a time, so this runs
    on the critical path of the conversation: it has to come back in well under
    the length of the audio it was handed."""
    authorize(authorization)
    if _stt is None:
        raise HTTPException(status_code=503, detail="the speech-to-text model is still loading")

    raw = file.file.read()
    # An empty upload is a hang-up racing the last chunk, not an error.
    if not raw:
        return {"text": ""}
    # Whisper invents speech when handed a sliver of audio — a trailing buffer
    # flushed at end-of-utterance comes back as "thanks for watching!" and
    # reaches the agent as though the caller had said it. Below a fifth of a
    # second there is nothing a caller could have said anyway.
    if wav_duration(raw) < MIN_TRANSCRIBE_SECONDS:
        return {"text": ""}

    # The model name on the wire is Patter's ("whisper-1"): it names the
    # protocol, not the weights, which are whichever ones Factor installed.
    with _stt_lock:
        segments, info = _stt.transcribe(
            io.BytesIO(raw),
            language=(language or LANGUAGE) or None,
            # The same defence one layer down: drop anything that is not
            # speech before the decoder gets a chance to imagine words for it.
            vad_filter=True,
            condition_on_previous_text=False,
        )
        segments = [s for s in segments if not is_hallucination(s)]

    text = " ".join(segment.text for segment in segments).strip()
    if response_format != "verbose_json":
        return {"text": text}
    return {
        "task": "transcribe",
        "language": getattr(info, "language", language or LANGUAGE),
        "duration": getattr(info, "duration", 0.0),
        "text": text,
        "segments": [
            {
                "id": index,
                "start": segment.start,
                "end": segment.end,
                "text": segment.text,
                # Patter reads avg_logprob back out as exp(avg_logprob) to
                # score confidence, so it has to survive the trip.
                "avg_logprob": segment.avg_logprob,
                "no_speech_prob": segment.no_speech_prob,
            }
            for index, segment in enumerate(segments)
        ],
    }


@app.post("/v1/audio/speech")
def speech(payload: dict = Body(...), authorization: str | None = Header(default=None)):
    """Synthesise one agent reply.

    Piper renders at its voice's own rate — 22.05 kHz for the medium voices —
    and the caller is promised 24 kHz, so everything is resampled on the way
    out. Skipping that lands every voice about nine percent flat."""
    authorize(authorization)
    if _tts is None:
        raise HTTPException(status_code=503, detail="no text-to-speech voice is configured")

    text = (payload.get("input") or "").strip()
    response_format = payload.get("response_format") or "pcm"
    if not text:
        raise HTTPException(status_code=400, detail="input is required")

    with _tts_lock:
        pcm, rate, width, channels = synthesize(text)

    if rate != OUTPUT_RATE:
        pcm, _ = audioop.ratecv(pcm, width, channels, rate, OUTPUT_RATE, None)
    if channels != 1:
        pcm = audioop.tomono(pcm, width, 0.5, 0.5)

    if response_format == "wav":
        return Response(content=wav_container(pcm, OUTPUT_RATE, width), media_type="audio/wav")
    # "pcm" is what Patter asks for: headerless signed 16-bit little-endian.
    return Response(content=pcm, media_type="audio/pcm")


def wav_duration(raw: bytes) -> float:
    """Length of an uploaded WAV in seconds, 0 if it cannot be read."""
    try:
        with wave.open(io.BytesIO(raw), "rb") as wav:
            return wav.getnframes() / float(wav.getframerate() or 1)
    except (wave.Error, EOFError):
        return 0.0


def is_hallucination(segment) -> bool:
    """Reject a segment the decoder is not really confident it heard.

    Both signals matter: no_speech_prob catches the confident-sounding phrase
    invented over silence, and avg_logprob catches the mumbling that comes back
    from line noise. Neither is worth putting in front of the model as though
    the caller had said it."""
    if getattr(segment, "no_speech_prob", 0.0) > MAX_NO_SPEECH_PROB:
        return True
    return getattr(segment, "avg_logprob", 0.0) < MIN_AVG_LOGPROB


def synthesize(text: str) -> tuple[bytes, int, int, int]:
    """Render text through Piper, returning raw PCM and its shape."""
    pcm = bytearray()
    rate, width, channels = OUTPUT_RATE, 2, 1
    for chunk in _tts.synthesize(text):
        pcm.extend(chunk.audio_int16_bytes)
        rate, width, channels = chunk.sample_rate, chunk.sample_width, chunk.sample_channels
    return bytes(pcm), rate, width, channels


def wav_container(pcm: bytes, rate: int, width: int) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wav:
        wav.setnchannels(1)
        wav.setsampwidth(width)
        wav.setframerate(rate)
        wav.writeframes(pcm)
    return buf.getvalue()


# ------------------------------------------------------------------- preparing

# Piper publishes every voice it has in one catalogue. Reading it at install
# time is what lets Factor speak a language without shipping a table that goes
# stale the moment Piper adds a voice.
VOICES_CATALOGUE = "https://huggingface.co/rhasspy/piper-voices/resolve/main/voices.json?download=true"

# Preference order when a language has several voices. Medium is the sweet spot
# — 48 of Piper's 49 language families have one, at about 63 MB — and the
# smaller tiers keep the one that does not (and any low-memory machine) served.
QUALITY_ORDER = ("medium", "low", "x_low", "high")

# Where a language has an obvious reference voice, name it: the catalogue's own
# ordering would otherwise break ties on file size, which is noise. Anything not
# listed — the long tail this design exists to serve — is resolved from the
# catalogue, and a name that ever leaves it falls through to the same path.
PREFERRED_VOICES = {
    # A bare language picks the locale most of its speakers expect, rather than
    # whichever one the catalogue happens to list first.
    "en": "en_US-lessac-medium",
    "en_US": "en_US-lessac-medium",
    "en_GB": "en_GB-alba-medium",
    "es": "es_ES-davefx-medium",
    "es_ES": "es_ES-davefx-medium",
    "es_MX": "es_MX-ald-medium",
    "fr": "fr_FR-siwis-medium",
    "fr_FR": "fr_FR-siwis-medium",
    "de": "de_DE-thorsten-medium",
    "de_DE": "de_DE-thorsten-medium",
    "it": "it_IT-serena-medium",
    "it_IT": "it_IT-serena-medium",
    "pt": "pt_BR-faber-medium",
    "pt_BR": "pt_BR-faber-medium",
    "nl": "nl_NL-mls-medium",
    "nl_NL": "nl_NL-mls-medium",
    "pl": "pl_PL-darkman-medium",
    "pl_PL": "pl_PL-darkman-medium",
    "ru": "ru_RU-irina-medium",
    "ru_RU": "ru_RU-irina-medium",
    "zh": "zh_CN-huayan-medium",
    "zh_CN": "zh_CN-huayan-medium",
}


def fetch_catalogue() -> dict:
    import urllib.request

    with urllib.request.urlopen(VOICES_CATALOGUE, timeout=60) as response:
        return json.loads(response.read().decode())


def resolve_voice(language: str, catalogue: dict) -> str | None:
    """Pick the best Piper voice for a language.

    Matching widens deliberately: an exact locale first (es_MX), then any voice
    in the same family (es_ES for "es"), so a caller who asks for a locale
    Piper has never heard of still gets their language rather than English."""
    family = language.split("-")[0].split("_")[0].lower()
    wanted = language.replace("-", "_").lower()

    preferred = {code.lower(): voice for code, voice in PREFERRED_VOICES.items()}

    def quality_rank(entry: dict) -> int:
        quality = entry.get("quality", "")
        return QUALITY_ORDER.index(quality) if quality in QUALITY_ORDER else len(QUALITY_ORDER)

    exact, loose = [], []
    for key, entry in catalogue.items():
        code = (entry.get("language", {}).get("code") or "").lower()
        if code == wanted:
            exact.append((key, entry))
        elif code.split("_")[0] == family:
            loose.append((key, entry))

    def best(bucket: list) -> str | None:
        # The long tail this design exists to serve. Rank by quality, then by
        # name: deterministic, and it does not chase a smaller file that
        # happens to be a worse voice.
        if not bucket:
            return None
        return min(bucket, key=lambda item: (quality_rank(item[1]), item[0]))[0]

    # The locale the caller asked for always outranks their language at large,
    # so es-MX is answered in Mexican Spanish rather than the Castilian
    # reference voice.
    for candidate in (preferred.get(wanted), best(exact), preferred.get(family), best(loose)):
        if candidate and candidate in catalogue:
            return candidate
    return None


def pick_whisper(explicit: str) -> tuple[str, str, str]:
    """Choose the transcription model this machine can actually keep up with.

    Whisper decodes a fixed thirty-second window however little audio it is
    given, so cost is per call, not per second of speech — and Patter calls
    once a second. Measured on this design: small takes ~2.4 s per chunk on a
    CPU and falls further behind with every word, while on CUDA it takes
    ~0.14 s. base keeps up on a CPU at ~0.9 s, which is why it is the fallback
    rather than the recommendation."""
    device, compute = "cpu", "int8"
    try:
        import ctranslate2

        if ctranslate2.get_cuda_device_count() > 0:
            device, compute = "cuda", "float16"
    except Exception:  # noqa: BLE001 - no CUDA is the common case, not an error
        pass

    if explicit:
        return explicit, device, compute
    if device == "cuda":
        return "small", device, compute
    # No GPU: accuracy has to give way to keeping up, because a transcriber
    # that falls behind does not produce a late answer, it produces a growing
    # backlog and a caller talking to nobody.
    return ("base" if total_ram_gb() >= 2 else "tiny"), device, compute


def total_ram_gb() -> float:
    try:
        return os.sysconf("SC_PAGE_SIZE") * os.sysconf("SC_PHYS_PAGES") / (1024**3)
    except (ValueError, OSError, AttributeError):
        return 8.0


def prepare() -> None:
    """Install everything a local tier needs, then report what was chosen.

    Factor runs this once, at setup, and writes the answer into the config —
    so the first phone call finds the weights already on disk and the choices
    already made."""
    result: dict = {}
    model, device, compute = pick_whisper(CFG.get("whisper_model") or "")
    result["whisper_model"] = model
    result["whisper_device"] = device
    result["whisper_compute"] = compute
    if CFG.get("need_stt") and device == "cpu":
        # Say it plainly rather than let the user discover it on a call: this
        # machine can run local transcription, but not well.
        result["warning"] = (
            f"no GPU here, so transcription runs {model} on the CPU — it keeps up, but it "
            "mishears more than the cloud does, especially outside English. Cloud "
            "speech-to-text with a local voice (tier 3) is the better trade on this machine."
        )

    voice = CFG.get("piper_voice") or ""
    if CFG.get("need_tts"):
        if not voice:
            log("resolving a voice", language=LANGUAGE)
            voice = resolve_voice(CFG.get("language") or "en", fetch_catalogue())
            if not voice:
                die(f"Piper has no voice for {CFG.get('language')!r}; use the cloud tier for speech")
        log("downloading the voice", voice=voice)
        from piper.download_voices import download_voice

        target = DATA_DIR / "piper"
        target.mkdir(parents=True, exist_ok=True)
        download_voice(voice, target)
    result["piper_voice"] = voice

    if CFG.get("need_stt"):
        log("downloading the transcription model", model=model, device=device)
        mute_onnx_telemetry()
        from faster_whisper import WhisperModel

        # Constructing it is what pulls the weights; doing it here means the
        # download happens under the installer's progress, not mid-call.
        WhisperModel(model, device=device, compute_type=compute,
                     download_root=str(DATA_DIR / "whisper"))

    print(json.dumps(result), flush=True)


def main() -> None:
    if "--prepare" in sys.argv:
        prepare()
        return

    import uvicorn

    uvicorn.run(
        app,
        host=CFG.get("host") or "127.0.0.1",
        port=int(CFG.get("port") or 8726),
        log_level="warning",
    )


if __name__ == "__main__":
    main()
