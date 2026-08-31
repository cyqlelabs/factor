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
| 📓 **Learns from its own work** | A turn that took four or more tool calls becomes a skill it writes for itself once the session goes quiet ([Extending](#extending-factor)) |
| 🔧 **Self-managing** | Edits its own config, installs packages, upgrades and restarts itself, schedules cron jobs and one-off reminders, runs `HEARTBEAT.md` checks that cost nothing when idle |
| 🛡️ **Safety rails** | Workspace-restricted files, exec deny-patterns, sender allowlists, scrubbed secrets — rails, not a sandbox ([Security](#security-model)) |

## How it works

<!-- A PNG, not a mermaid fence: the GitHub mobile app shows mermaid as raw code.
     Source: docs/assets/architecture.mmd — rebuild with: make diagrams -->

<p align="center">
  <img src="https://raw.githubusercontent.com/cyqlelabs/factor/main/docs/assets/architecture.png" width="625"
       alt="Telegram and the CLI reach the message bus, which feeds the agent loop. The phone reaches the loop through the voice shell sidecar and PC voice through the mic and speakers, both running turns directly. The loop recalls from and stores to the smrti REST sidecar, calls the provider chain, and drives the tool registry and its suites. Jobs, cron and the heartbeat publish proactive results onto the bus.">
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
can read the prompts, tool schemas, replies and token counts it actually sends. On a
desktop, the running gateway also puts a status icon in the system tray: version,
uptime, memory health, connected channels, and a clean quit.

<details>
<summary><b>Proxy details, the memory sidecar, and how upgrades land</b></summary>

Loopback stays direct and child processes inherit the proxy setting, so smrti's calls
show up but the local sidecars aren't caught. `--proxy-ca` trusts an intercepting
proxy's CA, probed once at startup. The browser isn't routed; it has its own trust
store. The tray is absent on a headless box and on macOS, whose tray would cost the
build its CGO-free binaries.

Factor supervises the smrti sidecar, restarts it with backoff, and degrades
gracefully (empty recalls, dropped writes) when it's down. Point
`memory.mode: "external"` + `memory.url` at a shared smrti if you run one.

`factor upgrade` downloads the release for this machine, verifies it against the
published `SHA256SUMS`, and swaps the binary in place (`--check` only reports). A
running gateway restarts into it once the turn in flight is answered, keeping its
pid so systemd never sees it stop. Factor checks daily and tells you, never
installing unasked.

The same command brings smrti up to date however it runs here: a container is
recreated on the newly published image, and a uv, pipx, pip or venv install is
upgraded by the installer that made it, then restarted into. Both wait for the
memory graph to go quiet first, so nothing in flight is lost. An engine on
another machine is left to whoever runs it.

</details>

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
    "max_tool_iterations": 20,
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
    "utility": [{ "type": "ollama", "model": "qwen3:8b" }]  // cheaper chain for compaction summaries and skill verdicts; omit = the main one
  },
  "memory": {
    "mode": "sidecar",                       // sidecar | external | off
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
    "command": "",                           // "" = find one; init records what it installed
    "headless": false,
    "fast_path": false                       // opt in to the lightweight read-only engine
  },
  "heartbeat": { "enabled": true, "interval_minutes": 30 },
  "trace": {
    "enabled": true,                         // one JSON line per turn in ~/.factor/traces
    "record_args": false,                    // the shape of a turn, not what was said to the tools
    "keep_days": 14
  },
  "upgrade": { "check": true, "check_interval_hours": 24 },  // report new releases; never install one unasked
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

The workspace (`~/.factor/workspace`) is the agent's home. The persona is built into
the binary, so an upgrade improves it everywhere at once; `SOUL.md` layers yours on
top, `USER.md` holds what Factor should always know about you, `AGENT.md` tunes how
it works, `HEARTBEAT.md` lists proactive tasks, and `instructions/`, `skills/`,
`sessions/`, `cron/` do what they say.

## Browser

Factor drives a real browser over DevTools: it attaches to your running
Chrome/Chromium/Brave, or launches a managed instance that stays visible so you can
watch it work. A machine with none gets [Helium](https://helium.computer) installed
under `~/.factor/engine` — ungoogled-chromium with uBlock Origin bundled, from a
portable tarball that needs no package manager and no root.

Reading a page and driving a page cost wildly different amounts, so you can add a
second engine for the cheap half:

| Engine | Tools | Renders | Good for |
|---|---|---|---|
| **Chromium** — Helium, or the browser you already run | `browser_navigate` · `_read` · `_scroll` · `_click` · `_fill` · `_keys` · `_upload` · `_tabs` · `_screenshot` · `_eval` · `_back` | yes | anything interactive |
| **Lightpanda** — opt-in, `browser.fast_path` | `browser_fetch` — title, text, links | never | reading a page for a fraction of the memory |

**Browse as yourself.** Start your everyday browser with
`--remote-debugging-port=9222` (or point `browser.attach_url` at it) and Factor uses
that session instead of launching one — your logins, your cart, your cookies — so
sites that turn away a fresh automated profile serve it normally.

`browser_read` puts main content first and site furniture last, says how much it
withheld, and takes `filter`/`limit`; `browser_scroll` reaches what only loads on
the way down. Lightpanda keeps no session and can't click, fill or screenshot, so it
supplements the real browser rather than replacing it; its builds need glibc 2.34,
which the wizard checks before the 150 MB download.

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
**[Telnyx](docs/phone-telnyx.md)**.

The voice shell is [Patter](https://github.com/PatterAI/Patter), installed on demand
and supervised like the smrti sidecar. It owns the carrier's media stream,
turn-taking, barge-in, voice activity detection, answering-machine detection and
transcoding; Factor is its brain, over an endpoint that never leaves `127.0.0.1`.

**Speech tiers** — the one decision with real trade-offs, asked by the wizard:

| Tier | Speech-to-text | Text-to-speech | Extra RAM | When |
|---|---|---|---|---|
| **1 · cloud** (default) | Deepgram nova-3 | ElevenLabs flash v2.5 (µ-law 8 kHz, no transcode) | ~150–300 MB | any machine; lowest latency, least to go wrong |
| **2 · local STT** | Parakeet / Whisper | ElevenLabs | +0.5–2 GB | transcription stays home |
| **3 · local TTS** | Deepgram | Piper | +0.3 GB | Piper's ~100 ms render beats the cloud, on any CPU |
| **4 · fully local** | Parakeet / Whisper | Piper | +1–2 GB | no audio leaves the machine, no per-minute audio cost |

A tier picks who serves each half of the pipeline; everything else is the same call:

<!-- A PNG, not a mermaid fence: the GitHub mobile app shows mermaid as raw code.
     Source: docs/assets/speech-tiers.mmd — rebuild with: make diagrams -->

<p align="center">
  <img src="https://raw.githubusercontent.com/cyqlelabs/factor/main/docs/assets/speech-tiers.png" width="581"
       alt="The voice shell hears with a speech-to-text engine and speaks with a text-to-speech one. Tiers 2 and 4 hear with Parakeet or Whisper and tiers 3 and 4 speak with Piper, both on Factor's own speech server; tiers 1 and 3 hear with Deepgram and tiers 1 and 2 speak with ElevenLabs, off the machine.">
</p>

Pick a local tier and Factor installs it: engines in their own virtualenv, your
language's models on disk before setup finishes, nothing to start by hand.

Roughly $0.04–0.06 per talk-minute on tier 1 plus your model's tokens, and about
1.3¢ per SMS segment. Simple questions land in 1.5–3 s. The reply arrives whole, but
a tool-using turn still speaks: the line Factor says on its way to the answer is
streamed into the live call while the tools run.

<details>
<summary><b>Which languages and voices you get, and which transcriber</b></summary>

Transcription covers Whisper's ~99 languages, with Parakeet serving its 25 at higher
accuracy where the machine allows. Voices come from Piper's catalogue of **49**,
resolved from your `language` setting with the exact locale winning where it exists
(`es-MX` gets a Mexican voice, not a Castilian one). You can also pick the voice by
name: the wizard lists your ElevenLabs voices on the cloud tier and the catalogue's on
the local one, and a voice named in `speech_server.piper_voice` is downloaded on the
next start.

`speech_server.speech_speed` paces it: 0.9 speaks a tenth slower, 1.1 a tenth faster.
Piper stretches the phonemes rather than the audio, so the voice keeps its pitch —
past about 1.2 either way it stops sounding like a person.

Whisper decodes a fixed 30-second window however little audio it gets, and the phone
pipeline feeds it about a second at a time — so its cost is per chunk, not per second
of speech. Measured on this design: `small` takes ~2.4 s per 1 s chunk on a CPU and
falls behind while you talk; `base` keeps up at ~0.9 s but mishears more. That used
to be the CPU's ceiling.

[Parakeet TDT 0.6B v3](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3) breaks the
trade: a transducer's cost scales with the audio it is handed, not a fixed window,
and its accuracy benchmarks at Whisper large-v3 level. So the installer picks:

| Machine | Transcriber |
|---|---|
| GPU | Whisper `large-v3-turbo` — every language, large-class accuracy |
| CPU, ≥4 GB RAM, one of Parakeet's 25 languages | Parakeet TDT int8 (~1 GB resident) |
| CPU otherwise | Whisper `base`/`tiny`, as before |

`speech_server.stt_engine` (`parakeet` / `whisper`) or `speech_server.whisper_model`
override the choice. On a machine that lands on `base`, tier 3 — Piper's ~100 ms
render locally, transcription in the cloud — is still the better trade, and the
wizard says so.

</details>

<details>
<summary><b>Bring your own speech server</b></summary>

Point `stt.base_url` / `tts.base_url` at anything OpenAI-compatible —
[Speaches](https://github.com/speaches-ai/speaches) or your own — and Factor uses it
as-is. It probes at startup either way and falls back to the cloud tier if the
server isn't answering; set `local_audio_fallback: false` to have the channel report
itself down instead. Silero voice-activity detection runs locally in every tier.

</details>

<details>
<summary><b>Getting the line up, and the guardrails on it</b></summary>

Buy a number at Twilio or Telnyx, then run `factor init`. The wizard asks which one,
takes the credentials, and verifies them live before writing anything.

| | Twilio | Telnyx |
|---|---|---|
| Credentials | account SID + auth token | API key + connection id + public key |
| Setup at the carrier | buy a number | buy a number, create a Call Control Application |
| Cost | the baseline above | lower per minute and per text |
| Step by step | [docs/phone-twilio.md](docs/phone-twilio.md) | [docs/phone-telnyx.md](docs/phone-telnyx.md) |

The carrier is pointed at the shell on every start — Patter does it for Twilio,
Factor for Telnyx — so a rotating tunnel keeps working with nothing to click.

The rails are closed by default, because a number is dialable by anyone and every
minute costs money: only `user_number` may call in, only `user_number` may be
dialed, calls are cut off at `max_call_minutes`, and transfer is off. Two tools
appear once the channel is configured — `phone_sms` sends a text and `phone_call`
dials, reporting the outcome with a transcript tail into the conversation that asked.

Your carrier's page has the rest: the allowlist knobs that widen those rails, how to
move off the default Cloudflare quick tunnel — fine for a first call, wrong for
daily use — and what to check when the line does not come up.

</details>

## PC voice: mic and speakers

The same conversation without a phone bill: Factor listens on the machine's own
microphone and answers through its speakers. The mic opens whenever Factor runs — in
`factor` (the terminal chat keeps working alongside) and in `factor gateway` alike.

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

`factor talk` works in every mode: it rescues a misfired wake word and cuts off
whatever is playing, and `/talk` does the same inside the chat. Talk over a reply and
it stops mid-word, cancelling the turn behind it. The speech tiers are the phone's,
chosen the same way, with the local server on its own port so both channels can keep
speech at home.

**It hears people, not clips.** Two people talking to each other leave gaps far
shorter than the silence that closes a recording, so their whole exchange arrives as
one clip — and a single embedding of that is a blend that hands both of them one
name. With `speaker_id` on, the speech server separates the voices first and reads
each one alone. The owner keeps the main conversation, a recognized guest gets a
session of their own and is named to the agent as the person speaking, and
`voice_speakers` turns a profile born `speaker-2` into Roxana.

**And it knows who is listening.** That, not who asked, is the question
confidentiality turns on. A second voice in a recording makes the room shared — at
most one of them is the owner, so the rest are company — which holds before anyone is
enrolled and even when nobody could be named.

| Room | Session | Recalls from | Remembers into |
|---|---|---|---|
| private | `voice:local`, or the guest's own | `space` + `shared_space` | `space` |
| shared | `voice:local:room` — everyone in one thread | `shared_space` | `shared_space` |

One utterance declares company; `room_timeout_minutes` of silence or the `room` tool
takes it back. The asymmetry is deliberate: a room wrongly called shared costs you a
coy answer, one wrongly called private says something private to a guest.

<details>
<summary><b>Microphone, meter and barge-in</b></summary>

Audio rides the sound system's own helpers — `parec`/`paplay`, `pw-record`/`pw-play`,
`arecord`/`aplay`, sox's `rec`/`play` on macOS, sox's `waveaudio` driver on Windows —
installed by the wizard, which also asks *which* microphone and proves it live: you
make a noise, it measures, and a silent source is called out on the spot. The chat's
status bar carries a live meter — `mic ▂▄▁` moves with the room and turns green on
speech, `♪` lights cyan while Factor talks, a dead source shows `mic ✗`.

Voice activity detection is pure Go: adaptive noise floor, a pre-roll so the first
syllable survives, and a higher bar while the agent speaks so the speakers can't
barge in on themselves. Speakers loud enough get past that bar anyway, so what got
through is matched against the words Factor just sent them: its own voice is dropped
instead of answered, or stripped off the front of what you actually said.
`output_volume` turns the reply down in rooms where the speakers overpower the
microphone.

A sentence the wake word never reached gets a soft two-note chime, so a misfire sounds
different from a machine that never heard you. It is a tone and not a voice, so talking
across it is not an interruption and still needs the wake word; it waits fifteen seconds
between chimes, because a conversation held in front of the machine is turned away
sentence by sentence. `ignored_chime` turns it off.

</details>

<details>
<summary><b>How a voice becomes a name</b></summary>

Each voice is matched against the profiles in `~/.factor/voice-speakers.json`, which
needs a local speech tier — that is what computes the embeddings. The turn belongs to
whoever opened the recording, since the wake word was at its front; anyone who joined
halfway through is the room, not the asker. `unknown_speaker` decides what a new voice
gets: a profile on the spot, or the main conversation, unnamed.

Three bars rise with the stakes: one second of speech to put a name to a voice, two
to fold it into that profile, three to create one, because a spurious profile never
goes away and competes for every match after it. Every decision lands in the log with
the similarity behind it, so a turn answered as the wrong person is readable rather
than a guess.

The room is read from every utterance the mic resolves, including the ones the wake
word turned away — someone talking to *you* is still someone in the room. Sound only
reports people who make it, so the `room` tool is how you mention someone who came in
quietly or left. Either flip is spoken before the answer that depends on it, and the
room outlives the process, aged against the wall clock rather than uptime, so a 9pm
upgrade doesn't bring Factor back up private while the guest is still on the sofa.

</details>

<details>
<summary><b>Replies you can listen to</b></summary>

A spoken turn is told it's being heard rather than read, so replies come out sayable
— no markdown, no bullet lists, no spelled-out URLs. `voice_write` sends anything
long or written to your terminal instead, or to the chat you last used when Factor
runs as a daemon.

The local tier keeps to itself. Factor sets `ORT_DISABLE_TELEMETRY=1` in the speech
process's environment before it starts, because onnxruntime otherwise uploads your
OS build, CPU, memory and a persistent device id as it initializes, and its own
`disable_telemetry_events()` runs too late to stop it
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
calls; once that session sits quiet for ten minutes, it spends one metered call
asking whether the trajectory holds a workflow worth keeping. Most of the time
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
```

The suite runs against fakes — scripted providers, a fake smrti sidecar and a fake
voice shell (both spawned by re-execing the test binary), a fake Telegram API, a
fake carrier, a scripted microphone and speaker, a fake MCP server over real stdio
JSON-RPC, a scripted desktop — plus live headless-Chrome and desktop round-trip
tests that auto-skip where the machine can't host them.

Decisions worth the argument, and the alternatives they beat, live in
[`docs/decisions/`](docs/decisions).

## License

MIT © CyqleLabs
