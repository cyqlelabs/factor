<!-- Plain <img> on purpose: the GitHub mobile app does not render
     <picture>/<source>, it drops the element and shows a broken-image icon.
     The src is an absolute raw URL for the same reason: the mobile app does
     not resolve repo-relative image paths either. -->

<p align="center">
  <img src="https://raw.githubusercontent.com/cyqlelabs/factor/main/docs/assets/logo.png" width="128" alt="Factor logo">
</p>

<h1 align="center">Factor</h1>

<p align="center">
  <a href="https://github.com/cyqlelabs/factor/actions/workflows/ci.yml"><img src="https://github.com/cyqlelabs/factor/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI"></a>
  <a href="https://github.com/cyqlelabs/factor/actions/workflows/ci.yml"><img src="https://img.shields.io/endpoint?url=https%3A%2F%2Fgist.githubusercontent.com%2Fwebpolis%2F180b17501295a75b23122fc6c9eca223%2Fraw%2Ffactor-coverage.json" alt="Coverage"></a>
  <a href="https://pkg.go.dev/github.com/cyqlelabs/factor"><img src="https://pkg.go.dev/badge/github.com/cyqlelabs/factor.svg" alt="Go reference"></a>
  <a href="https://github.com/cyqlelabs/factor/releases/latest"><img src="https://img.shields.io/github/v/release/cyqlelabs/factor" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/cyqlelabs/factor" alt="MIT license"></a>
</p>

**A fast, lightweight AI agent that lives on your machine — with hands on your
desktop, a voice on the phone, and a real memory.**

Factor is a single static Go binary. Talk to it in the terminal, on Telegram, over
the phone, or out loud to the machine itself — same agent, same tools, same memory
every time. It drives a real browser, works your desktop, runs long tasks in the
background, and calls or texts you when one lands. Long-term memory is
[smrti](https://github.com/cyqlelabs/smrti): it consolidates and prioritizes rather
than logging, so past failures become constraints it doesn't repeat.

## Highlights

| | |
|---|---|
| 🧠 **Memory as the soul** | Salience-ranked recall every turn, consolidation that decays and prunes, plus `remember` / `recall` / `forget` / `reflect` tools |
| ⚡ **Never keeps you waiting** | Says what it's about to do while the tools run; long work becomes a background job that messages you when it lands |
| 🎯 **Mid-turn steering** | A second message during a live turn is injected between tool iterations, not queued |
| ❓ **Asks when only you know** | `ask_user` puts the question where you are: the chat the turn came from, the terminal you're already in, or a dialog on your desktop — and times out rather than hanging the turn when you're away |
| 🔁 **Provider failover** | OpenAI-compatible (OpenRouter, Ollama, LM Studio, Groq, llama.cpp, …) and native Anthropic, with per-candidate cooldowns and overflow compaction |
| 💸 **Counts what it spends** | Every call priced and billed to its session — status bar, tray, `usage` tool — with per-session and global caps; cache reads and writes priced at their own rates, not as fresh input |
| ♻️ **Caches the prompt prefix** | The same assembly order every turn, with cache breakpoints where two turns first differ, so the provider reuses the longest prefix it can — and a turn twenty tool calls deep stops reprocessing its own history |
| 🧭 **Reasoning, dialect-translated** | One `provider.reasoning` setting becomes `reasoning`, `reasoning_effort`, or a `thinking` budget |
| ☎️ **Answers the phone** | A real number to call, or it calls and texts you — barge-in, voicemail detection, optional fully local speech ([Phone](#phone-calls-and-sms)) |
| 🎙️ **Listens in the room** | Mic in, speakers out, barge-in, optional wake word, push-to-talk via `factor talk` ([PC voice](#pc-voice-mic-and-speakers)) |
| 👥 **Tells voices apart** | A recording is read voice by voice, so each person holds their own conversation — and company in the room moves the answer to a memory space it can hear ([PC voice](#pc-voice-mic-and-speakers)) |
| 🖐️ **Hands on your desktop** | Windows, screenshots, mouse, keyboard, clipboard, notifications on X11/Wayland/macOS/Windows — plus grid vision ([Desktop](#desktop)) |
| 🌐 **A real browser, not just fetch** | CDP tools attach to your running Chrome/Chromium/Brave or launch a managed one ([Browser](#browser)) |
| 🧩 **Extensible everything** | Channel connectors, Go tools, runtime-mounted MCP servers, markdown skills ([Extending](#extending-factor)) |
| 📊 **Watches its own numbers** | Every turn leaves a local trace — models, tools, timings, cache and cost — and control bands measure it against a rolling baseline, so the heartbeat wakes a model only once a number has drifted |
| 📓 **Learns from its own work** | A turn that took four or more tool calls, or one you had to steer, becomes a skill it writes for itself once the session goes quiet ([Extending](#extending-factor)) |
| 🔧 **Self-managing** | Edits its own config, installs packages, upgrades and restarts itself, schedules cron jobs and one-off reminders, runs `HEARTBEAT.md` checks that cost nothing when idle |
| 🛡️ **Safety rails** | Workspace-restricted files, exec deny-patterns, sender allowlists, scrubbed secrets — rails, not a sandbox ([Security](#security-model)) |

## How it works

<!-- A PNG, not a mermaid fence: the GitHub mobile app shows mermaid as raw code.
     Source: docs/assets/architecture.mmd — rebuild with: make diagrams -->

<p align="center">
  <img src="https://raw.githubusercontent.com/cyqlelabs/factor/main/docs/assets/architecture.png" width="625"
       alt="Telegram and the CLI reach the message bus, which feeds the agent loop. The phone reaches the loop through the voice shell sidecar and PC voice through the mic and speakers, both running turns directly. The loop recalls from and stores to the smrti REST sidecar, calls the provider chain, asks the local decision model for typed decisions, and drives the tool registry and its suites. Jobs, cron and the heartbeat publish proactive results onto the bus.">
</p>

Bus + bounded workers, mid-turn steering, narrow pluggable seams, CGO-free
portability — [PicoClaw](https://github.com/sipeed/picoclaw)'s architecture in a
codebase that runs happily on an old Puppy Linux box.

## Get started

```bash
go install github.com/cyqlelabs/factor/cmd/factor@latest
# or grab a release binary; linux-amd64 targets GOAMD64=v1 (no SSE4.2 needed)

factor init      # interactive setup wizard
```

On Windows, download
[factor-windows-amd64.msi](https://github.com/cyqlelabs/factor/releases/latest/download/factor-windows-amd64.msi)
and run it. It installs for your account alone, so it asks for no administrator,
puts `factor` on your PATH and in the Start menu, and offers to start the setup
wizard when it finishes. Remove it in Settings > Apps; your settings and memory in
`%USERPROFILE%\.factor` stay.

The wizard verifies every step live — a real provider completion, the endpoint's
model list, Telegram's `getMe`, carrier and voice credentials, an actual page load —
and installs what's missing: smrti, a browser, your desktop backend's helpers. It
probes the machine's display rather than this shell, so setup over ssh targets the
right desktop, and it can add a login entry (systemd or XDG autostart, launchd, the
Windows Run key). `factor init -y` takes the defaults; `--no-install` installs
nothing.

```bash
export FACTOR_PROVIDER_API_KEY=sk-or-...   # OpenRouter by default
factor                                     # interactive chat
factor -m "what's on my disk?"             # one-shot
factor gateway                             # daemon: Telegram, phone, PC voice, cron, heartbeat, jobs
factor gateway -d                          # the same, detached (~/.factor/gateway.log)
factor talk                                # push-to-talk: arm the PC voice microphone
factor status                              # daemon / provider / memory / phone / voice / desktop health
factor upgrade                             # replace this binary with the newest release
factor -p 127.0.0.1:8080                   # route HTTP through a proxy and watch every call
```

`-p` routes Factor's HTTP through any proxy — mitmproxy, Burp, ZAP, SOCKS5 — so you
can read the prompts, tool schemas, replies and token counts it actually sends;
`--proxy-ca` trusts an intercepting proxy's certificate. A gateway started by systemd
or your login gets no flags, so give it `proxy.address` and `proxy.ca` in the config.

`factor upgrade` verifies the newest release against its `SHA256SUMS`, swaps the
binary and updates smrti; a running gateway restarts into it once the current turn is
answered.

On a Linux or Windows desktop, the running gateway also puts a status icon in the
system tray: version, uptime, memory health, connected channels, and a clean quit.

## Configuration

`~/.factor/config.json` — every key optional, defaults work. `FACTOR_*` env
overrides: `FACTOR_PROVIDER_API_KEY`, `FACTOR_PROVIDER_MODEL`, `FACTOR_MEMORY_MODE`, …

A running gateway watches the file. Save an edit, by hand or through the agent's own
`config_set`, and it reloads within seconds — after the turn in flight is answered,
and without restarting the sidecars — then names the changed sections in the chat it
reports back to. A save that doesn't parse is warned about and retried, never applied.

<details>
<summary><b>Annotated example</b></summary>

```jsonc
{
  "log_level": "info",                       // debug | info | warn | error
  "agent": {
    "context_window_tokens": 0,              // 0 = ask the model catalog; a value only ever shrinks its answer
    "max_tool_iterations": 20,                // per stretch; a turn still mid-task is checkpointed and gets up to three
    "summarize_at_percent": 75,              // how full the window gets before compaction
    "keep_recent_messages": 8,               // what survives it
    "learn_skills": true,                    // distill a finished multi-tool turn into a skill
    "version_workspace": false               // keep a local git history of the workspace, so an edit can be undone
  },
  "provider": {
    "type": "openrouter",                    // openrouter|openai|groq|ollama|lmstudio|llamacpp|anthropic|custom
    "api_key": "sk-or-...",
    "model": "google/gemini-3.1-pro-preview",
    "reasoning": { "effort": "xhigh" },      // or {"max_tokens": 12000}; "none" turns it off
    "fallbacks": [{ "type": "ollama", "model": "qwen3:8b" }],
    "utility": [{ "type": "ollama", "model": "qwen3:8b" }],  // cheaper chain for compaction summaries and skill verdicts; omit = the main one
    "light": { "type": "openrouter", "model": "deepseek/deepseek-v4-flash-0731" }  // fast model for the progress notes of a long turn; omit = picked for you
  },
  "memory": {
    "max_rss_mb": 0,                         // restart the engine for size once idle; 0 sizes it to the machine, -1 turns it off
    "mode": "sidecar",                       // sidecar | external (set "url" to a shared smrti) | off
    "auto_install": true,                    // install smrti when it is missing
    "personality": "balanced",               // analytical | curious | empathetic | maverick | deterministic
    "space": "main",                         // where conversations are remembered
    "space_strategy": "origin",              // origin: cron and job turns use system_space | single: one space for all
    "system_space": "system",
    "shared_space": "shared"                 // what a turn other people can hear reads and writes
  },
  "channels": {
    "telegram": { "token": "123:ABC", "allow_from": ["your-telegram-id"] },
    "phone": {                                 // optional; absent = nothing runs
      "user_number": "+15550001111",           // you: the only one who may call in
      "phone_number": "+15550002222",          // the number you bought
      "carrier": "twilio",                     // twilio | telnyx
      "twilio_account_sid": "AC...",           // twilio: these two
      "twilio_auth_token": "...",
      // telnyx instead: "telnyx_api_key", "telnyx_connection_id", "telnyx_public_key"
      "elevenlabs_api_key": "...",
      "stt_api_key": "...",                    // Deepgram
      "language": "en",
      "stt": { "provider": "deepgram" },       // deepgram | whisper | local-openai
      "tts": { "provider": "elevenlabs" },     // elevenlabs | local-openai
      "proactive": "sms",                      // sms | call | off
      "max_call_minutes": 15
    },
    "voice": {                                 // PC voice: this machine's mic and speakers
      "activation": "wake-word",               // always | wake-word | push-to-talk
      "wake_word": "factor",
      "language": "en",
      "stt": { "provider": "deepgram" },       // deepgram | whisper | local-openai
      "stt_api_key": "...",                    // Deepgram
      "tts": { "provider": "elevenlabs" },     // elevenlabs | local-openai
      "elevenlabs_api_key": "...",
      "speaker_id": false,                     // tell the room apart; needs a local speech tier
      "speaker_threshold": 0.35,               // similarity below which a voice is nobody enrolled
      "unknown_speaker": "anonymous",          // anonymous | enroll
      "room_isolation": null,                  // null = on wherever speaker_id is
      "room_timeout_minutes": 30,              // how long a voice counts as still in the room
      "output_volume": 100,                    // 1–100; lower it when the speakers reach the mic
      "ignored_chime": true                    // a soft tone when it heard you and did not take it
    }
  },
  "mcp": {
    "servers": { "github": { "command": "github-mcp-server", "args": ["stdio"] } }
  },
  "tools": { "disabled": [], "restrict_to_workspace": true },
  "desktop": { "enabled": null },            // null = on when a display exists
  "browser": {
    "enabled": true,
    "engine": "auto",                        // auto | camofox | chromium
    "command": "",                           // "" = find one; init records what it installed
    "headless": false,
    "camofox": { "port": 9377 }              // the headless engine's sidecar; one already answering here is adopted
  },
  "heartbeat": { "enabled": true, "interval_minutes": 30 },
  "cron": { "job_timeout_minutes": 30 },     // how long one scheduled task may run; it is told what is left at each checkpoint
  "trace": {
    "enabled": true,                         // one JSON line per turn in ~/.factor/traces
    "record_args": false,                    // the shape of a turn, not what was said to the tools
    "keep_days": 14
  },
  "decision": {                              // typed decisions; see "Typed decisions"
    "mode": "active",                        // active | shadow (ask, record, act on nothing) | off
    "engine": "auto",                        // auto | laya | student; auto picks the student without AVX2 or under 4 GB RAM
    "min_confidence": 0.6,                   // the bar a verdict clears to be acted on
    "thresholds": { "completion": 0.75 },    // per kind: operation | target | completion | recovery | induction
    "browser": true, "verify": false, "recover": true, "induce": true, // the scenarios, each switchable; verify is off until asked for
    "cache_entries": 512,                    // memoize repeated decisions; 0 turns it off
    "device": "",                            // "" = let the runtime choose; a name pins the onnxruntime provider
    "auto_install": true                     // build the private virtualenv when it is missing
  },
  "upgrade": { "check": true, "check_interval_hours": 24 },  // report new releases; never install one unasked
  "proxy": { "address": "", "ca": "" },      // "" = direct; host:port or a URL routes every call, sidecars included
  "cost": {
    "track": true,                           // price every call; models served locally cost nothing
    "budget": {
      "session_usd": 0,                      // 0 = no cap, on both scopes
      "global_usd": 0,
      "period": "month"                      // what "global" counts: day | month | total
    },
    "prices": {}                             // USD per million tokens, for models the catalog does not list
  }
}
```

</details>

Spend is priced from the model catalog cached in `~/.factor/pricing.json` and
totalled in `~/.factor/usage.json`, per session and overall. Locally served models
are free; models the catalog doesn't list are counted in tokens rather than guessed
at. Caps are checked before the call, and the turn answers with a line saying what
stopped. Ask for `usage` to see the breakdown.

A cap is a stop line rather than a hard ceiling, and the difference is worth
knowing: the totals are re-read from disk on every check, so a gateway and a
terminal spend against the same number, and calls already in flight are reserved at
what a call has cost on average so far — but the call that crosses the cap finishes
and is billed in full, because what a call costs is not known until it answers. A
model nothing prices moves no cap at all; the usage report names those.

The workspace (`~/.factor/workspace`) is the agent's home. The persona is built into
the binary, so an upgrade improves it everywhere at once; `SOUL.md` layers yours on
top, `USER.md` holds what Factor should always know about you, `AGENT.md` tunes how
it works, `HEARTBEAT.md` lists proactive tasks, and `instructions/`, `skills/`,
`sessions/`, `cron/` do what they say.

## Browser

Two engines answer the same eleven tools — `browser_navigate` · `_read` · `_scroll`
· `_click` · `_fill` · `_keys` · `_upload` · `_tabs` · `_screenshot` · `_eval` ·
`_back` — and `browser.engine` picks between them:

| Engine | What it is | Runs when |
|---|---|---|
| **Chromium** — Helium, or the browser you already run | a real browser driven over DevTools, visible on the desktop | a display exists, or a browser of yours is attached on port 9222 |
| **Camofox** — [camofox-browser](https://github.com/jo-inc/camofox-browser) over [Camoufox](https://camoufox.com) | a Firefox build whose fingerprint is spoofed in C++ before any page script runs, so Cloudflare and the sites that turn a headless Chromium away serve it | there is no display to open a window on, or `browser.headless` is set |

`auto` is that table; `camofox` and `chromium` force one engine. **Browse as
yourself.** Start your everyday browser with `--remote-debugging-port=9222` (or point
`browser.attach_url` at it) and Factor uses that session instead of launching one —
your logins, your cart, your cookies — and it wins over both engines.

Nothing here is installed by hand. `factor init` puts both engines down — Helium for a
machine with no browser, Camofox on every machine — and a gateway that finds Camofox
missing installs it in the background as it starts, so an install upgraded from a Factor
without it has the engine before the first page is asked for. The Camofox install is an
npm package and a 300 MB Firefox build kept under `~/.factor/engine/camofox`, run on the
machine's Node 22.13 or newer, or on a Node Factor downloads for Linux, macOS or Windows
and checks against its published checksum. Two of the package's native bindings want a
glibc newer than the distributions Factor is most often put on; where they will not
load, Factor stands pure-JavaScript stand-ins in for them (the browser itself needs
nothing newer than glibc 2.18), so no compiler is ever needed. It runs as a sidecar on
port 9377 — one you run yourself is adopted — and idles at a sleeping Node process,
launching Firefox on the first page and shutting it down when nothing has asked for a
while. Its crash telemetry is switched off.

`browser_read` says how much it withheld and takes `filter`/`limit`; on Camofox a page
comes back as an accessibility snapshot with element refs, a tenth the size of the HTML,
and `offset` reads on past a cut. `browser_scroll` reaches what only loads on the way
down. A twelfth tool, `browser_run`, works a page toward a stated goal in one call on
the Chromium engine (see [Typed decisions](#typed-decisions)).

No step in the suite sleeps for a fixed time after a click, a submit, a scroll or a
back: it probes the page before and after, and reads as soon as the page has changed
and held still, so a same-document toggle is read in a frame and a slow navigation is
not read at its old URL.

## Typed decisions

Which of thirty controls to click, whether a reply's "done" is backed by its tool
results, whether a trajectory is worth a skill: choices among candidates the code
already enumerated, not generation. They go to
[Laya](https://github.com/NandhaKishorM/laya), a 322M-parameter Apache-2.0 encoder
running **on your machine** — nothing to sign up for, pay for or configure. Factor
installs it (~350 MB on disk, an int8 graph served by onnxruntime) and supervises
it like the memory engine, skipping it on a box with under a gigabyte free.

Laya is too slow on a CPU without AVX2 and too big for a machine with under 4 GB of
RAM. On those machines a six-layer student distilled from it answers instead, served
by the memory engine in about a seventh of the memory. `decision.engine` forces
either model. `browser_run` still needs Laya: the student knows only the fixed
questions it was trained on, and every page offers a new list of controls.

| What it buys | |
|---|---|
| **`browser_run`** | one call works a page toward a goal; never submits, books, pays or sends without `allow_submit` |
| **Stalls** | a call that keeps returning the same result is named as wait, re-read, change route or ask you |
| **Completion** | a reply claiming work the tool results do not show is handed back |
| **Skill induction** | SKIP is decided before the model that writes the skill is paid |

Answers are validated against the candidates offered and must clear a confidence
bar; under it — or while the model is installing, or down — the agent does what it
did before. `decision.mode` is `active`, `shadow` (records every verdict in
`~/.factor/traces`, acts on none) or `off`, which switches off the memory engine's
decisions too.

## Desktop

Factor works the graphical session through the desktop's own helper programs
(xdotool, wmctrl, scrot on X11; grim/wtype on Wayland; osascript on macOS;
PowerShell on Windows) — no CGO bindings, and nothing registers on a headless box.

On top of the window/mouse/keyboard/clipboard tools sits **grid vision**, a two-pass
pointing loop for vision-capable models:

1. `screen_view` captures the screen under a battleship coordinate grid — columns
   A, B, C…, rows 1, 2, 3…. The model names the cell it sees ("the icon is in D4")
   instead of guessing pixel coordinates, which vision models are bad at.
2. `screen_zoom cell=D4` magnifies that cell — or any pixel region, e.g. a window's
   geometry from `window_list` — under a finer sub-grid, down to ~10px precision.
3. `mouse action=click cell=B3` clicks the cell's center, resolved back to native
   screen pixels on either view.

Pure Go image math — no OpenCV, no OCR, no helpers beyond the screenshot program.
Frames are capped at 1568px on the longest side (clicks still land at native
resolution), only the two newest stay in context, and image bytes never touch
session history. Non-vision models can disable both vision tools via
`tools.disabled`.

**On a Mac, grant Screen Recording first.** Without it `screencapture` does not
fail — it exits 0 and returns the wallpaper with every window removed, so a
desktop with six applications open reads as an empty one. `screen_view` says which
permission produces that; it lives under System Settings → Privacy & Security →
Screen Recording.

## Phone calls and SMS

Give Factor a phone number and it picks up: you talk, it answers out loud, with the
same memory, tools, and history it has everywhere else. It can also call or text
*you* — a finished job, a cron result, or because you asked it to ring someone.

```bash
factor init        # the Channels step walks through the carrier and the speech tier
factor gateway     # brings the line up
factor status      # number, speech tier, voice-shell health
```

Carrier setup has a page of its own: **[Twilio](docs/phone-twilio.md)** ·
**[Telnyx](docs/phone-telnyx.md)**. Telnyx costs less per minute and per text but
needs a Call Control Application; the wizard verifies either carrier's credentials
live before writing anything.

The voice shell is [Patter](https://github.com/PatterAI/Patter), installed on demand
and supervised like the smrti sidecar. It handles the call audio, turn-taking,
barge-in and voicemail detection; Factor answers over an endpoint that never leaves
`127.0.0.1`.

**Speech tiers** — the one decision with real trade-offs, asked by the wizard:

| Tier | Speech-to-text | Text-to-speech | Extra RAM | When |
|---|---|---|---|---|
| **1 · cloud** (default) | Deepgram nova-3 | ElevenLabs flash v2.5 (µ-law 8 kHz, no transcode) | ~150–300 MB | any machine; lowest latency, least to go wrong |
| **2 · local STT** | Parakeet / Whisper | ElevenLabs | +0.5–2 GB | transcription stays home |
| **3 · local TTS** | Deepgram | Piper | +0.3 GB | Piper's ~100 ms render beats the cloud, on any CPU |
| **4 · fully local** | Parakeet / Whisper | Piper | +1–2 GB | no audio leaves the machine, no per-minute audio cost |

<!-- A PNG, not a mermaid fence: the GitHub mobile app shows mermaid as raw code.
     Source: docs/assets/speech-tiers.mmd — rebuild with: make diagrams -->

<p align="center">
  <img src="https://raw.githubusercontent.com/cyqlelabs/factor/main/docs/assets/speech-tiers.png" width="581"
       alt="The voice shell hears with a speech-to-text engine and speaks with a text-to-speech one. Tiers 2 and 4 hear with Parakeet or Whisper and tiers 3 and 4 speak with Piper, both on Factor's own speech server; tiers 1 and 3 hear with Deepgram and tiers 1 and 2 speak with ElevenLabs, off the machine.">
</p>

Pick a local tier and Factor installs it, models included. To use your own server
instead, point `stt.base_url` / `tts.base_url` at anything OpenAI-compatible, such as
[Speaches](https://github.com/speaches-ai/speaches); one that isn't answering at
startup falls back to the cloud tier unless `local_audio_fallback` is `false`.

Roughly $0.04–0.06 per talk-minute on tier 1 plus your model's tokens, and about
1.3¢ per SMS segment. Simple questions land in 1.5–3 s, and a tool-using turn says
what it's doing while the tools run.

The rails are closed by default, because anyone can dial a number and every minute
costs money: only `user_number` may call in or be dialed, calls end at
`max_call_minutes`, and transfer is off. `phone_sms` sends a text and `phone_call`
dials, reporting the outcome and a transcript tail into the conversation that asked.
Your carrier's page covers widening the rails, moving off the default Cloudflare
quick tunnel, and what to check when the line doesn't come up.

<details>
<summary><b>Languages, voices and transcribers</b></summary>

Transcription covers Whisper's ~99 languages, with
[Parakeet](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3) serving its 25 more
accurately where the machine allows. Voices come from Piper's 49, matched to your
`language` setting with the exact locale winning (`es-MX` gets a Mexican voice, not a
Castilian one), or picked by name in the wizard. `speech_server.speech_speed` paces
the voice — 0.9 a tenth slower, 1.1 a tenth faster — without changing its pitch.

The installer picks the transcriber:

| Machine | Transcriber |
|---|---|
| GPU | Whisper `large-v3-turbo` — every language, large-class accuracy |
| CPU, ≥4 GB RAM, one of Parakeet's 25 languages | Parakeet TDT int8 (~1 GB resident) |
| CPU otherwise | Whisper `base` or `tiny` |

`speech_server.stt_engine` (`parakeet` / `whisper`) or `speech_server.whisper_model`
override the choice. Whisper `base` mishears more, so on those machines tier 3 —
local Piper, cloud transcription — is the better trade, and the wizard says so.

</details>

## PC voice: mic and speakers

The same conversation without a phone bill: Factor listens on the machine's own
microphone and answers through its speakers. The mic opens in `factor` and
`factor gateway` alike, and the terminal chat keeps working beside it.

```bash
factor init        # the Channels step sets up the mic, the speech tier, and the activation
factor             # or factor gateway — either one listens
factor talk        # push-to-talk: arm the microphone from any terminal
factor status      # tier, activation, helpers, and whether anything is listening
```

| `activation` | It answers |
|---|---|
| `always` | every utterance — best alone in a quiet room |
| `wake-word` | utterances that open with the wake word, plus a short window after each reply so follow-ups don't need it (the wizard preselects this) |
| `push-to-talk` | nothing until `factor talk` arms the microphone |

`factor talk` (or `/talk` in the chat) works in every mode and rescues a missed wake
word. Talk over a reply and Factor stops mid-word and cancels the turn behind it. The
speech tiers are the [phone's](#phone-calls-and-sms).

**It tells voices apart.** With `speaker_id` on, which needs a local speech tier, the
speech server splits each recording by speaker before matching voices to profiles.
The owner keeps the main conversation, a recognized guest gets one of their own, and
`voice_speakers` turns a profile born `speaker-2` into Roxana.

**And it knows who is listening.** A second voice in one recording, or about three
seconds of a voice that isn't yours over a couple of minutes, makes the room shared.
Everyone present then shares one conversation, and private memory stays out of it:

| Room | Session | Recalls from | Remembers into |
|---|---|---|---|
| private | `voice:local`, or the guest's own | `space` + `shared_space` | `space` |
| shared | `voice:local:room` | `shared_space` | `shared_space` |

Factor says so before answering each time the room flips. It turns private again
only after `room_timeout_minutes` of silence or when you tell it everyone left (the
`room` tool), because a room wrongly called private says a secret to a guest. Tell it
somebody walked in and it asks your question again in the shared session. Jobs and
scheduled tasks answer into the room as it is when they finish.

The split covers memory only. `AGENT.md`, `USER.md` and `instructions/` reach every
turn whoever is listening, so in company the model is asked to be discreet about
them rather than kept from them.

<details>
<summary><b>Microphone, echo and spoken replies</b></summary>

The wizard installs the audio helpers (`parec`/`paplay`, `pw-record`/`pw-play`,
`arecord`/`aplay`, or sox on macOS and Windows), asks which microphone to use, and
tests it live. The chat's status bar carries a meter: `mic ▂▄▁` turns green on
speech, `♪` lights while Factor talks, and `mic ✗` marks a dead source.

Factor recognizes its own voice when the speakers reach the mic and drops it; lower
`output_volume` if they still overpower the mic. A sentence it heard but didn't take,
such as one without the wake word, gets a soft chime; `ignored_chime` turns it off.

Replies come out speakable — no markdown, lists or URLs — and `voice_write` sends
anything long to the terminal, or to your last chat when Factor runs as a daemon.
Speech starts once the first sentence is rendered, not the whole reply. The local
speech server runs with onnxruntime's telemetry switched off
([onnxruntime#25573](https://github.com/microsoft/onnxruntime/issues/25573)).

</details>

## Extending Factor

| Seam | What it takes |
|---|---|
| **Connector** | One package: `channel.Register(name, factory)` in `init()`, with its own config section |
| **Tool** | Four methods — `Name`, `Description`, `Parameters`, `Execute` — and one `registry.Register(t)` line |
| **MCP server** | `mcp_add` (or the `mcp.servers` config section) mounts its tools at runtime — no Go required |
| **Skill** | Drop `workspace/skills/<name>/SKILL.md` — catalog in prompt, full text on demand, `skill_find` searches the public registry (skills.sh) and `skill_install` takes its slug, a git URL, or a directory |

**It also writes its own.** Factor remembers a turn that took four or more tool
calls, or two if you steered it while it ran, since a correction names both the
approach that was wrong and the one that worked; once that session sits quiet for
ten minutes, it spends one metered call asking whether the trajectory holds a
workflow worth keeping. Most of the time
the answer is `SKIP`. A `LEARN` lands as a skill in the same catalog, marked
`learned: true`, and the next turn lists it like any other. Induction rewrites
its own output and never a skill you wrote or installed — `skill_write` drops the
marker, which is how a learned skill graduates out of its reach. The learned
library holds 40, so past that induction has to improve an entry rather than
mint a near-duplicate. `agent.learn_skills: false`, or `FACTOR_LEARN_SKILLS=0`,
turns it off.

<details>
<summary><b>Wiring in a connector</b></summary>

```go
func init() {
    channel.Register("mychat", func(raw json.RawMessage, b *bus.MessageBus) (channel.Channel, error) {
        var cfg MyConfig
        _ = json.Unmarshal(raw, &cfg)          // your own config section
        return New(cfg, b), nil                // implement Name/Start/Stop/Send/MaxMessageLength
    })
}
```

</details>

## Security model

Factor is a personal agent, not a multi-tenant service. The guardrails (workspace
restriction, exec deny-patterns, allowlists, secret redaction) protect against
accidents and casual prompt-injection — they are **not** a security boundary. Run it
under your own account for yourself; set `channels.telegram.allow_from`; keep
`restrict_to_workspace` on unless you know why you're turning it off. The phone
channel is the one place the default is *closed* rather than open — a number anyone
can dial, with a bill attached to every minute, earns stricter rails.

## Development

```bash
make check        # gofmt + vet + race tests + coverage gate (≥90%, what CI runs)
make lint         # golangci-lint (its own CI job, so check alone is not the gate)
make hooks        # point git at .githooks: every commit lints first
make build        # local binary
make build-all    # release cross-compile (incl. GOAMD64=v1 for old x86-64)
make build-tiny   # -tags nobrowser: smallest binary
make diagrams     # re-render docs/assets/*.mmd into the PNGs the docs embed (needs node)
```

The suite runs against fakes — scripted providers, a fake smrti sidecar and a fake
voice shell (both spawned by re-execing the test binary), a fake Telegram API, a
fake carrier, a scripted microphone and speaker, a fake MCP server over real stdio
JSON-RPC, a scripted desktop — plus live headless-Chrome and desktop round-trip
tests that auto-skip where the machine can't host them.

## Code signing policy

Windows releases are signed. Free code signing provided by
[SignPath.io](https://about.signpath.io), certificate by
[SignPath Foundation](https://signpath.org).

| Role | Members |
|---|---|
| Committers and reviewers | [CyqleLabs members](https://github.com/orgs/cyqlelabs/people) |
| Approvers | [CyqleLabs owners](https://github.com/orgs/cyqlelabs/people?query=role%3Aowner) |

Each signed file comes from this repository's release workflow, built from the
tagged source, and an approver releases every signing request by hand.

### Privacy

Factor sends your conversations only where you point it: to the language-model
providers you configure, to the channels you turn on (Telegram, phone, voice
services), and to the sites, searches and servers its tools use for your requests.
The memory engine runs on your machine and sends text to the same provider to
extract facts from it. Factor has no telemetry, and it switches off the telemetry
of every engine, library and browser it runs.

On its own, Factor contacts these services, sending none of your data:

| Service | Why | Turned off by |
|---|---|---|
| GitHub | new Factor releases; the decision model's weights | `upgrade.check: false`; `decision.mode: off` |
| PyPI | installing and updating the memory engine and the decision runtime | `memory.auto_install: false`; `decision.auto_install: false` |
| Hugging Face | the memory engine's embedding and entity models | `memory.mode: off` |
| npm, nodejs.org | installing the browser engine | `browser.engine: chromium` |
| Your package manager (winget, apt, Homebrew) | installing the media player | `tools.allow_install: false` |
| openrouter.ai | its public price list | `cost.track: false`, with no budget caps set |

## License

MIT © CyqleLabs
