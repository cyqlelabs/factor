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

Factor is a single static Go binary. Chat with it in the terminal, message it on
Telegram, call its phone number, or just talk to the machine itself: the same agent
picks up every time, with the same tools and the same memory. It drives a real browser, works your
desktop, keeps long tasks running in the background, and calls or texts you when one
lands. Its long-term memory is [smrti](https://github.com/cyqlelabs/smrti) — Bayesian
truth values, attention economics, emotional valence — so Factor doesn't just log
what you said: it consolidates, prioritizes, and *never repeats a critical mistake*.

## Highlights

| | |
|---|---|
| 🧠 **Memory as the soul** | Salience-ranked recall every turn; past failures become hard constraints; consolidation decays, promotes, and prunes — plus deliberate `remember` / `recall` / `forget` / `reflect` tools |
| ⚡ **Never keeps you waiting** | A turn opens with a line on what Factor is about to do, sent while the tools are still running — and long work becomes a background job it acks instantly, then messages you when the result lands, even mid-conversation |
| 🎯 **Mid-turn steering** | A second message during a live turn is injected between tool iterations instead of queuing |
| 🔁 **Provider failover that works** | OpenAI-compatible (OpenRouter, Ollama, LM Studio, Groq, llama.cpp, …) and native Anthropic, with error classification, per-candidate cooldowns, and overflow-triggered compaction |
| 💸 **Counts what it spends** | Every provider call is priced from the live model catalog and billed to the session that made it — in the chat status bar, in the tray, and in a `usage` tool — with per-session and global caps that refuse the next call rather than the one already paid for |
| 🧭 **Reasoning, dialect-translated** | One `provider.reasoning` setting becomes `reasoning` (OpenRouter), `reasoning_effort` (OpenAI/Groq), or a `thinking` budget (Anthropic) |
| ☎️ **Answers the phone** | A real number: call it and talk to Factor out loud, or have it call and text you — barge-in, voicemail detection, and a fully local speech tier if you want no audio leaving the machine ([Phone](#phone-calls-and-sms)) |
| 🎙️ **Listens in the room** | Talk to the machine itself: mic in, speakers out, with barge-in, an optional wake word, and push-to-talk via `factor talk` — on the same speech tiers as the phone ([PC voice](#pc-voice-mic-and-speakers)) |
| 🖐️ **Hands on your desktop** | Windows, screenshots, mouse, keyboard, clipboard, notifications — X11, Wayland, macOS, Windows; auto-registered when a display exists — plus **grid vision**: a vision model sees the screen under a coordinate grid, zooms a cell, and clicks it by name ([Desktop](#desktop)) |
| 🌐 **A real browser, not just fetch** | CDP tools attach to your running Chrome/Chromium/Brave or launch a managed instance, visible by default so you can watch it work — and setup installs one when the machine has none ([Browser](#browser)) |
| 🧩 **Extensible everything** | Channel connectors, Go tools, runtime-mounted MCP servers, markdown skills, drop-in instructions — see [Extending](#extending-factor) |
| 🔧 **Self-managing** | Edits its own config, installs packages (apt/dnf/pip/npm/…), upgrades and restarts itself into the newest release, runs cron schedules and `HEARTBEAT.md` checks that cost zero LLM calls when idle |
| 🛡️ **Safety rails** | Workspace-restricted files, exec deny-patterns, sender allowlists, secrets scrubbed from every tool result — rails, not a sandbox ([Security](#security-model)) |

## How it works

```mermaid
flowchart LR
    TG([Telegram]) <--> BUS
    CLI([CLI]) <--> BUS
    PH([Phone]) <--> SHELL["voice shell sidecar · speech · barge-in"]
    SHELL <-->|chat completions on loopback| LOOP
    PC([PC voice]) <--> MIC["mic + speakers · VAD · wake word · barge-in"]
    MIC <-->|synchronous turns| LOOP
    BUS[message bus] --> LOOP["agent loop · one live turn per session"]
    LOOP <-->|recall · store| MEM[("smrti REST sidecar")]
    LOOP --> PROV["provider chain · cost meter · failover · cooldowns · compaction"]
    LOOP --> REG[tool registry]
    REG --- SUITES["fs · exec · web · browser · desktop · memory · jobs · cron · config · pkg · skills · MCP"]
    BG[jobs · cron · heartbeat] -.->|proactive results| BUS
```

Bus + bounded workers, mid-turn steering, narrow pluggable seams, CGO-free
portability — the architecture of [PicoClaw](https://github.com/sipeed/picoclaw)
distilled into a codebase that runs happily on an old Puppy Linux box.

## Get started

```bash
go install github.com/cyqlelabs/factor/cmd/factor@latest
# or grab a release binary; linux-amd64 targets GOAMD64=v1 (no SSE4.2 needed)

factor init      # interactive setup wizard
```

The wizard verifies every step live: the provider with a real completion, the model
picked from the endpoint's live list, the Telegram token with `getMe`, the carrier
and voice credentials against their own APIs, the browser with a real page load.
Then it installs what's missing instead of handing you a list — smrti (`uv` →
`pipx` → `pip --user` → private venv, no root needed), a browser, the helpers your
desktop backend wants. It looks for that desktop on the machine rather than in this
shell, so `factor init` over ssh still sets up the desktop the box is running.
It also asks whether Factor should start when you log in and, if so, puts the
entry in place itself: a systemd user service or XDG autostart on Linux, a
launchd agent on macOS, the Run key on Windows. `factor init -y` takes the
defaults for scripting; `--no-install` keeps it from installing anything.

```bash
export FACTOR_PROVIDER_API_KEY=sk-or-...   # OpenRouter by default
factor                                     # interactive chat
factor -m "what's on my disk?"             # one-shot
factor gateway                             # daemon: Telegram, phone, PC voice, cron, heartbeat, jobs
factor gateway -d                          # the same, detached from this terminal (~/.factor/gateway.log)
factor talk                                # push-to-talk: arm the PC voice microphone
factor status                              # daemon / provider / memory / phone / voice / desktop health
factor upgrade                             # replace this binary with the newest release
factor -p 127.0.0.1:8080                   # route HTTP through a proxy and watch every call
```

`-p` sends Factor's HTTP through any proxy — mitmproxy, Burp, ZAP, a corporate
egress, a SOCKS5 listener — so you can read the LLM traffic it is actually
sending: prompts, tool schemas, replies, token counts. Loopback is left direct,
so the memory and speech sidecars are not caught in the capture, and every child
process inherits the setting, which is how smrti's own extraction calls show up
too. A proxy that intercepts TLS needs its certificate authority trusted:
`--proxy-ca /path/to/ca.pem`, or leave it out and Factor looks where mitmproxy
and Burp put theirs. It probes once at startup, so a certificate it cannot trust
is a sentence on the way in rather than an x509 error an hour later. The browser
is not routed — it has its own trust store and its own proxy setting.

On a desktop, the running gateway puts an icon in the system tray. Click it
for a small status overview — version, uptime, memory health, connected
channels, a row each — and a quit item that stops the daemon cleanly. A headless box gets
no icon — and neither does macOS, whose tray would cost the build its CGO-free
binaries.

Factor spawns and supervises the smrti sidecar automatically, restarts it with
backoff, and degrades gracefully (empty recalls, dropped writes) when it's down.
Point `memory.mode: "external"` + `memory.url` at a shared smrti if you run one.

`factor upgrade` downloads the release built for this machine, checks it against the
published `SHA256SUMS`, and swaps the binary in place; `--check` only reports what's
out. A running gateway then restarts itself into the new binary — once it has
finished answering, and without changing pid, so systemd never sees it stop. It
looks for releases once a day and tells you in whichever chat you last used, but
never installs behind your back. Asking Factor to upgrade itself is the same path:
it installs, says goodbye, and messages you from the new binary a few seconds later.

## Configuration

`~/.factor/config.json` — every key optional, defaults work. `FACTOR_*` env
overrides: `FACTOR_PROVIDER_API_KEY`, `FACTOR_PROVIDER_MODEL`, `FACTOR_MEMORY_MODE`, …

<details>
<summary><b>Annotated example</b></summary>

```jsonc
{
  "provider": {
    "type": "openrouter",                    // openrouter|openai|groq|ollama|lmstudio|llamacpp|anthropic|custom
    "api_key": "sk-or-...",
    "model": "google/gemini-3.1-pro-preview",
    "reasoning": { "effort": "xhigh" },      // or {"max_tokens": 12000}; "none" turns it off
    "fallbacks": [{ "type": "ollama", "model": "qwen3:8b" }]
  },
  "memory": {
    "mode": "sidecar",                       // sidecar | external | off
    "auto_install": true,                    // install smrti when it is missing
    "personality": "balanced",               // analytical | curious | empathetic | maverick | deterministic
    "space": "main",                         // where conversations are remembered
    "space_strategy": "origin",              // origin: cron and job turns use system_space | single: one space for all
    "system_space": "system"
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
      "elevenlabs_api_key": "..."
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

Spend is priced from the model catalog Factor caches in `~/.factor/pricing.json` and totalled
in `~/.factor/usage.json`, per session and overall, so both survive a restart. A model this
machine serves is free; one the catalog does not list is counted in tokens and left unpriced
rather than guessed at. A cap is checked before the call it would pay for, and the turn answers
with a line saying what stopped instead of failing. Ask for `usage` to see the breakdown;
what the memory sidecar spends on its own extraction calls is not part of it.

The workspace (`~/.factor/workspace`) is the agent's home. Its persona is built into
the binary, so an upgrade improves it everywhere at once; `SOUL.md` layers your own
on top, `USER.md` holds what Factor should always know about you, and `AGENT.md`
tunes how it works. `HEARTBEAT.md` lists proactive tasks; `instructions/`, `skills/`,
`sessions/`, `cron/` do what they say.

## Browser

Factor drives a real browser over DevTools: it attaches to your running
Chrome/Chromium/Brave, or launches a managed instance that stays visible so you can
watch it work. A machine with no browser gets one — `factor init` installs
[Helium](https://helium.computer) under `~/.factor/engine`, from a portable tarball
that needs no package manager and no root. Helium is ungoogled-chromium with the
telemetry stripped, the anti-fingerprinting patches in, and uBlock Origin bundled,
which is what actually keeps a tab's memory down on a small box.

Reading a page and driving a page cost wildly different amounts, so you can add a
second engine for the cheap half:

| Engine | Tools | Renders | Good for |
|---|---|---|---|
| **Chromium** — Helium, or the browser you already run | `browser_navigate` · `_read` · `_scroll` · `_click` · `_fill` · `_screenshot` · `_eval` · `_back` | yes | anything interactive |
| **Lightpanda** — opt-in, `browser.fast_path` | `browser_fetch` — title, text, links | never | reading a page for a fraction of the memory |

**Browse as yourself.** Start your everyday browser with
`--remote-debugging-port=9222` (or point `browser.attach_url` at it) and Factor
attaches to that session instead of launching one of its own. It then sees the web
the way you do — your logins, your cart, your cookies — and the sites that turn
away a fresh automated profile serve it normally. A managed instance still works
for everything else, and says in the log which one it took.

A page read puts the main content first and the site navigation last, says how many
controls it showed out of how many exist, and takes `filter` to narrow them — so a
listing whose results sit behind two hundred nav links is still reachable, and a
page that was truncated cannot be mistaken for a page that was empty. `browser_scroll`
reaches the results that only load on the way down.

Lightpanda runs the same JavaScript against a DOM and never starts a renderer, a GPU
process, or a compositor. It cannot click, fill, or screenshot and it keeps no
session, so it supplements the real browser instead of replacing it — the agent
picks whichever the job needs. The wizard offers it only where it runs: its builds
need glibc 2.34, and the check happens before the 150 MB download, not after.

## Desktop

Factor works the graphical session through the desktop's own helper programs
(xdotool, wmctrl, scrot and friends on X11; grim/wtype on Wayland; osascript on
macOS; PowerShell on Windows) — no CGO bindings, so the binary stays static and the
tools cost nothing on a headless box, where they simply don't register.

On top of the plain window/mouse/keyboard/clipboard tools sits **grid vision**, a
two-pass pointing loop for vision-capable models:

1. `screen_view` captures the screen and attaches it with a battleship coordinate
   grid overlaid — columns A, B, C…, rows 1, 2, 3… The model doesn't guess pixel
   coordinates (which vision models are famously bad at); it names the cell it can
   see: "the icon is in D4".
2. `screen_zoom cell=D4` magnifies that cell (or any pixel region, e.g. a window's
   geometry from `window_list`) under a finer sub-grid, taking precision from
   ~cell-size down to ~10px in one more look.
3. `mouse action=click cell=B3` clicks the named cell's center — cells resolve back
   to native screen pixels automatically, on either view.

Everything is pure Go image math: no OpenCV, no OCR models, no extra helpers beyond
the screenshot program already required. Frames sent to the model are capped at
1568px on the longest side (clicks still land at native resolution), only the two
newest frames stay in context, and image bytes never touch session history — so a
long desktop session stays cheap in tokens and in disk, which is the point on a
small box. Non-vision models can keep the rest of the desktop suite and disable the
two vision tools via `tools.disabled`.

## Phone calls and SMS

Give Factor a phone number and it picks up: you talk, it answers out loud, with the
same memory, tools, and session history it has everywhere else. It can also call or
text *you* — a finished job, a cron result, or because you asked it to ring someone
and report back.

```bash
factor init        # the Channels step walks through the carrier and the speech tier
factor gateway     # brings the line up
factor status      # number, speech tier, voice-shell health
```

Setting up the carrier is a page of its own — portal steps, config, and what to check
when a call does not connect: **[Twilio](docs/phone-twilio.md)** ·
**[Telnyx](docs/phone-telnyx.md)**.

The voice shell is [Patter](https://github.com/PatterAI/Patter) running as a
supervised sidecar — exactly like the smrti memory engine, into its own virtualenv,
installed on demand. It terminates the carrier's media stream and owns the parts of
a phone call that are hard: turn-taking, barge-in, voice activity detection,
answering-machine detection, transcoding. Factor is its brain, plugged in over
loopback as an OpenAI-compatible endpoint that never leaves `127.0.0.1`.

**Speech tiers** — the one decision with real trade-offs, asked plainly by the wizard:

| Tier | Speech-to-text | Text-to-speech | Extra RAM | When |
|---|---|---|---|---|
| **1 · cloud** (default) | Deepgram nova-3 | ElevenLabs flash v2.5 (µ-law 8 kHz, no transcode) | ~150–300 MB | any machine; lowest latency, least to go wrong |
| **2 · local STT** | faster-whisper | ElevenLabs | +0.5–2 GB | transcription stays home; wants a GPU |
| **3 · local TTS** | Deepgram | Piper | +0.3 GB | Piper's ~100 ms render beats the cloud, on any CPU |
| **4 · fully local** | faster-whisper | Piper | +1–2 GB | no audio leaves the machine, no per-minute audio cost |

A tier picks who serves each half of the speech pipeline. Everything else is the same call:

```mermaid
flowchart LR
    SHELL["voice shell · Patter"] -->|hears with| STT{{speech-to-text}}
    SHELL -->|speaks with| TTS{{text-to-speech}}
    subgraph LOCAL ["Factor's own speech server · 127.0.0.1"]
        WH["faster-whisper"]
        PI[Piper]
    end
    subgraph CLOUD [audio leaves the machine]
        DG[Deepgram]
        EL[ElevenLabs]
    end
    STT -.->|tiers 2, 4| WH
    TTS -.->|tiers 3, 4| PI
    STT -.->|tiers 1, 3| DG
    TTS -.->|tiers 1, 2| EL
```

**Pick a local tier and Factor installs it.** The engines go into their own
virtualenv and your language's models download before setup finishes — so the first
call finds everything on disk. No server to start, no model names to choose.
`factor status` reports what it built.

**Languages and voices.** Transcription covers everything Whisper does, about 99
languages. Voices come from Piper's catalogue: **49 languages**, resolved from your
`language` setting. The exact locale wins where it exists — `es-MX` gets a Mexican
voice, not a Castilian one — then the language at large. Spanish is first-class on
every tier. You can also pick the voice yourself: the wizard lists your ElevenLabs
account's voices by name on the cloud tier and the catalogue's voices for your
language on the local one, and a voice named in `speech_server.piper_voice` is
downloaded on the next start if its weights aren't on disk yet.

<details>
<summary><b>Why a GPU changes which tier to pick</b></summary>

Whisper decodes a fixed 30-second window however little audio it gets, and the phone
pipeline feeds it about a second at a time. Cost is therefore per chunk, not per
second of speech. Measured on this design:

| Model | Device | Per 1 s chunk | Verdict |
|---|---|---|---|
| `small` | CUDA | ~0.14 s | keeps up comfortably |
| `base` | CPU | ~0.9 s | keeps up, mishears more |
| `small` | CPU | ~2.4 s | falls behind; the backlog grows while you talk |

So local transcription runs `small` on a GPU and drops to `base` on a CPU, and the
wizard says so when it does. **On a machine with no GPU, tier 3 is the better
trade**: Piper renders in ~100 ms on any CPU, and transcription stays in the cloud.

</details>

You can still point `stt.base_url` / `tts.base_url` at a speech server you run
yourself — [Speaches](https://github.com/speaches-ai/speaches), or anything else
OpenAI-compatible — and Factor will leave it alone and use it. Either way it probes
at startup. If the server is not answering, Factor falls back to the cloud tier and
says so rather than failing calls; set `local_audio_fallback: false` to have the
channel report itself down instead. Silero voice-activity detection runs locally in
every tier.

Roughly $0.04–0.06 per talk-minute on tier 1 plus your model's tokens, and about
1.3¢ per SMS segment. The reply lands whole rather than token by token, but a
tool-using turn still speaks: the line Factor says on its way to the answer is
streamed into the live call while the tools run. Simple questions land in the normal
1.5–3 s range.

<details>
<summary><b>Getting the line up, and the guardrails on it</b></summary>

Buy a number at Twilio or Telnyx, then run `factor init`. The wizard asks which one,
takes the credentials it needs, and verifies them — along with the voice key — live
before writing anything.

| | Twilio | Telnyx |
|---|---|---|
| Credentials | account SID + auth token | API key + connection id + public key |
| Setup at the carrier | buy a number | buy a number, create a Call Control Application |
| Cost | the baseline above | lower per minute and per text |
| Step by step | [docs/phone-twilio.md](docs/phone-twilio.md) | [docs/phone-telnyx.md](docs/phone-telnyx.md) |

Either way the carrier is pointed at the shell on every start — Patter does it for
Twilio, Factor itself for Telnyx — so a rotating tunnel keeps working and there is
nothing to click in the portal.

The carrier has to reach the voice shell, so it needs a public URL. `tunnel: "quick"`
(the default) uses Patter's built-in Cloudflare quick tunnel — fine for trying it
out, **not** for daily use: the hostname rotates and first legs occasionally drop.
For real use set `tunnel: "none"` and a stable `webhook_url` from a named tunnel or
a reverse proxy. Factor's own endpoints — the brain bridge and the shell's control
API — always bind `127.0.0.1` and share a bearer secret regenerated every boot.

Because a phone number is dialable by anyone and a phone call costs money, the rails
are closed by default: only `user_number` may call in (add more with `allow_from`, or
`"*"` for anyone, which logs a security warning), only `user_number` may be dialed
(add more with `allow_call_to` — there is deliberately no wildcard), calls are cut
off at `max_call_minutes`, and call transfer is off. A caller who is not allowed is
hung up at the carrier *and* refused by the bridge.

Two tools appear only when the channel is configured: `phone_sms` sends a text, and
`phone_call` dials — returning immediately, then reporting the outcome (answered,
no answer, busy, voicemail) with a transcript tail back into whichever conversation
asked for the call.

</details>

## PC voice: mic and speakers

The same conversation without a phone bill: Factor listens on the machine's own
microphone and answers through its speakers. The microphone opens whenever Factor
runs — in `factor` (the terminal chat keeps working alongside) and in
`factor gateway` alike.

```bash
factor init        # the Channels step sets up the mic, the speech tier, and the activation
factor             # or factor gateway — either one listens
factor talk        # push-to-talk: arm the microphone from any terminal
factor status      # tier, activation, helpers, and whether anything is listening
```

Audio goes through the sound system's own helpers — `parec`/`paplay` on PulseAudio
and PipeWire, `arecord`/`aplay` on bare ALSA, sox's `rec`/`play` on macOS, where
playback also falls back to the built-in `afplay` — and the wizard installs what's
missing. It also asks *which* microphone and then proves it live: you make a noise,
it measures the signal, and a source delivering silence — the usual fate of a
multichannel interface picked as the system default — is called out on the spot
instead of at the first ignored wake word. In the chat, the status bar carries a
live meter: `mic ▂▄▁` moves as the room does, turns green when it hears speech,
`♪` lights up cyan while Factor talks, and a dead source shows `mic ✗`. Voice activity detection is pure Go: an adaptive
noise floor, a pre-roll so the first syllable survives, and a higher bar while the
agent is speaking, so the speakers can't barge in on themselves. You still can:
talking over a reply stops it mid-word, and if a turn is still thinking, it is
cancelled — the new utterance owns the conversation. Windows capture isn't wired up
yet; the channel says so instead of pretending.

The local tier keeps to itself, and that took one fix: Piper runs its voices on
onnxruntime, which bundles Microsoft's telemetry client and posts your OS build,
CPU model, memory, network type and interpreter path to
`mobile.events.data.microsoft.com` on every model load. Factor switches it off
before Piper is imported. A speech tier chosen because it is local should not be
introducing itself to anyone.

A spoken turn is also told it is being heard rather than read, so the reply is
composed to be said — no markdown, no bullet lists, no URLs spelled out — instead
of being written for a screen and then stripped on the way to the speakers. The
stripping still happens as a seatbelt; `voice_write` is how anything long or
written reaches you in text instead.

Who it answers is the `activation` setting:

| Mode | It responds to |
|---|---|
| `always` | every utterance — best alone in a quiet room |
| `wake-word` | utterances that open with the wake word, plus a short window after each reply so follow-ups don't need it (the wizard preselects this) |
| `push-to-talk` | nothing until `factor talk` arms the microphone |

`factor talk` works in every mode: it is the rescue for a wake word that misfired,
and it cuts off whatever is playing. Inside the chat, `/talk` does the same without
leaving the prompt.

The speech tiers are the phone's, chosen the same way in the wizard — cloud
(Deepgram + ElevenLabs) or the managed local server (faster-whisper + Piper), which
runs on its own port so the phone and the PC can both keep speech local on one
machine. The voice is yours to pick either way: the wizard offers your ElevenLabs
account's voices by name, or Piper's catalogue for your language. One tool comes with the channel: ask for something in writing and the agent
uses `voice_write` — the text lands in your terminal, or in the chat you last used
when Factor runs as a daemon, and the spoken reply stays short.

## Extending Factor

| Seam | What it takes |
|---|---|
| **Connector** | One package: `channel.Register(name, factory)` in `init()`, with its own config section |
| **Tool** | Four methods — `Name`, `Description`, `Parameters`, `Execute` — and one `registry.Register(t)` line |
| **MCP server** | `mcp_add` (or the `mcp.servers` config section) mounts its tools at runtime — no Go required |
| **Skill** | Drop `workspace/skills/<name>/SKILL.md` — catalog in prompt, full text on demand, `skill_install` from git |

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
channel is the one place where the default is *closed* rather than open — a number
anyone can dial, and a bill attached to every minute, earn stricter rails.

## Development

```bash
make check        # gofmt + vet + race tests + coverage gate (≥90%, what CI runs)
make build        # local binary
make build-all    # release cross-compile (incl. GOAMD64=v1 for old x86-64)
make build-tiny   # -tags nobrowser: smallest binary
```

The suite runs against fakes — scripted providers, a fake smrti sidecar and a fake
voice shell (both spawned by re-execing the test binary), a fake Telegram API, a
fake carrier, a scripted microphone and speaker, a fake MCP server over real stdio
JSON-RPC, a scripted desktop — plus live headless-Chrome and desktop round-trip
tests that auto-skip where the machine can't host them.

## License

MIT © CyqleLabs
