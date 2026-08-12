# Factor

**A fast, reliable, lightweight desktop AI agent and companion — with a real memory.**

Factor is a single static Go binary that lives on your machine, talks to you over the
CLI or Telegram, does real work with real tools, and remembers what matters across
conversations. Its long-term memory is [smrti](https://github.com/cyqlelabs/smrti) —
an AtomSpace-inspired engine with Bayesian truth values, attention economics, and
emotional valence — so Factor doesn't just log what you said: it consolidates,
prioritizes, and *never repeats a critical mistake*.

Factor distills the architecture of [PicoClaw](https://github.com/sipeed/picoclaw)
(bus + bounded workers, mid-turn steering, narrow pluggable seams, CGO-free
portability) into a small, sharply focused codebase built for low-resource Linux
desktops — it runs happily on an old Puppy Linux box.

## Highlights

- **smrti memory as the soul** — every exchange is stored as episodes; every turn
  recalls salience-ranked memories into context. Past failures surface as hard
  behavioral constraints (`YOU MUST NOT …`), background facts as notes. Consolidation
  (decay, promotion, contradiction resolution, pruning) runs inside smrti's reflect
  epochs. The agent also has deliberate `remember` / `recall` / `forget` / `reflect`
  tools.
- **Never keeps you waiting** — long work runs through a background job engine
  (`job_start`, shell commands or delegated agent sub-tasks). The agent acks
  immediately; when a job finishes, the completion event re-enters the same session
  and Factor proactively messages you with the result — even mid-conversation.
- **One turn per session, steering for the rest** — a second message during a live
  turn is injected into that turn between tool iterations instead of queuing.
- **Provider failover that works** — OpenAI-compatible (OpenRouter, Ollama, LM Studio,
  Groq, llama.cpp, …) and native Anthropic backends, error classification, per-candidate
  cooldowns, context-overflow-triggered compaction at turn-safe boundaries.
- **Reasoning, dialect-translated** — one `provider.reasoning` setting (effort or an
  explicit token budget) reaches OpenRouter as `reasoning`, OpenAI/Groq as
  `reasoning_effort`, and Anthropic as a `thinking` budget. Defaults to `xhigh`.
- **Hands on your desktop** — `window_list` / `window_control` (focus, close,
  move, maximize…), `screenshot`, `mouse`, `type_text`, `press_key`,
  `clipboard`, `notify`, `open`, `desktop_info`. X11 and Wayland via the usual
  helpers (xdotool, wmctrl, scrot, xclip, grim, wl-clipboard), macOS via
  osascript/screencapture, Windows via PowerShell. Registered automatically
  when a graphical session exists, skipped on headless boxes.
- **A real browser, not just fetch** — CDP tools (`browser_navigate/read/click/fill/
  screenshot/eval/back`) that attach to your running Chrome/Chromium/Brave DevTools
  port, or launch a managed instance — visible by default so you can watch it work.
- **Extensible everything**:
  - *Connectors*: self-registering channel factories (Telegram included; WhatsApp,
    Twilio, Slack… are one package each).
  - *Tools*: one interface, one registration line; users curate the arsenal via
    `tools.disabled`.
  - *MCP*: built-in stdio client; `mcp_add` mounts any MCP server's tools at runtime
    and persists it to config.
  - *Skills*: markdown skills with progressive disclosure (catalog in prompt, full
    text on demand), `skill_install` from git or local dirs.
  - *Instructions*: `AGENT.md` / `SOUL.md` / `USER.md` plus drop-in
    `workspace/instructions/*.md`.
- **Self-managing** — `config_get` / `config_set` (redacted reads, schema-validated
  persisted writes), `pkg_install` (apt/apk/dnf/pacman/xbps/pkg/pip/pipx/uv/npm),
  cron schedules, HEARTBEAT.md proactive checks that cost zero LLM calls when idle.
- **Safety rails** — workspace-restricted file access with symlink-escape resolution,
  exec deny-patterns for catastrophic commands, sender allowlists, secrets scrubbed
  from every tool result. (Rails, not a sandbox — see [Security](#security-model).)

## Install

```bash
go install github.com/cyqlelabs/factor/cmd/factor@latest
# or grab a release binary; linux-amd64 targets GOAMD64=v1 (no SSE4.2 needed)

factor init      # interactive setup wizard
```

`factor init` is a terminal wizard: pick a provider from a menu, paste a key,
choose a model from the endpoint's **live model list**, set the reasoning
effort, then memory, channels and tools — each step verified as you go (the
provider with a real completion, the Telegram token with `getMe`).

It also **installs smrti**, Factor's memory engine, if it is missing — trying
`uv tool install`, `pipx`, `pip install --user` (retrying with
`--break-system-packages` on PEP-668 distros), and finally a private venv under
`~/.factor/venv`. Nothing needs root. Any later run installs it too if it went
missing (`memory.auto_install`), and the wizard offers to install the desktop
helpers your session lacks.

Scripting a machine? `factor init -y` takes the defaults and never prompts;
`--no-install` keeps it from installing anything.

## Quick start

```bash
export FACTOR_PROVIDER_API_KEY=sk-or-...   # OpenRouter by default
factor                                     # interactive chat
factor -m "what's on my disk?"             # one-shot
factor gateway                             # daemon: Telegram, cron, heartbeat, jobs
factor status                              # daemon / provider / memory health
```

Factor spawns and supervises `smrti serve rest` on localhost automatically
(`memory.mode: "sidecar"`), restarts it with backoff if it dies, and degrades
gracefully (empty recalls, dropped writes, clear health status) when it's down.
Point `memory.mode: "external"` + `memory.url` at a shared smrti if you run one.

## Configuration

`~/.factor/config.json` (all keys optional — defaults work). Environment overrides:
`FACTOR_HOME`, `FACTOR_PROVIDER_API_KEY`, `FACTOR_PROVIDER_MODEL`, `FACTOR_MEMORY_MODE`, …

```jsonc
{
  "provider": {
    "type": "openrouter",                    // openrouter|openai|groq|ollama|lmstudio|llamacpp|anthropic|custom
    "api_key": "sk-or-...",
    "model": "google/gemini-pro-latest",
    "reasoning": { "effort": "xhigh" },      // or {"max_tokens": 12000}; "none" turns it off
    "fallbacks": [{ "type": "ollama", "model": "qwen3:8b" }]
  },
  "memory": {
    "mode": "sidecar",                       // sidecar | external | off
    "auto_install": true,                    // install smrti when it is missing
    "personality": "balanced",               // analytical | curious | empathetic | maverick | deterministic
    "space": "main"
  },
  "channels": {
    "telegram": { "token": "123:ABC", "allow_from": ["your-telegram-id"] }
  },
  "mcp": {
    "servers": { "github": { "command": "github-mcp-server", "args": ["stdio"] } }
  },
  "tools": { "disabled": [], "restrict_to_workspace": true },
  "desktop": { "enabled": null },            // null = on when a display exists
  "browser": { "enabled": true, "headless": false },
  "heartbeat": { "enabled": true, "interval_minutes": 30 }
}
```

The workspace (`~/.factor/workspace`) is the agent's home: `AGENT.md`, `SOUL.md`,
`USER.md` shape its identity; `HEARTBEAT.md` lists proactive tasks; `instructions/`,
`skills/`, `sessions/`, `cron/` do what they say.

## Extending Factor

**A new connector** is one package:

```go
func init() {
    channel.Register("mychat", func(raw json.RawMessage, b *bus.MessageBus) (channel.Channel, error) {
        var cfg MyConfig
        _ = json.Unmarshal(raw, &cfg)          // your own config section
        return New(cfg, b), nil                // implement Name/Start/Stop/Send/MaxMessageLength
    })
}
```

**A new tool** implements four methods (`Name`, `Description`, `Parameters`,
`Execute`) and registers with `registry.Register(t)` — or skip Go entirely and add
an **MCP server** (`mcp_add`, or the `mcp.servers` config section) or a **markdown
skill** (`workspace/skills/<name>/SKILL.md`).

## Architecture

```
channels (telegram, cli, …)        smrti (Python sidecar, SQLite)
        │  ▲                                ▲  REST :8420
        ▼  │                                │
   message bus ──► agent loop ──► memory engine (recall → prompt, store ← turns)
        ▲          │    │ one turn per session; overflow = steering
        │          │    └► provider chain (failover, cooldowns, compaction)
        │          └────► tool registry
        │                  fs · exec · web · browser(CDP) · desktop(X11/Wayland/
        │                  macOS/Windows) · memory · jobs · cron
        │                  config · pkg · skills · MCP mounts
        └── job engine / cron / heartbeat re-enter the bus proactively
```

Design lineage and decisions are documented in [`tasks/plan.md`](tasks/plan.md).

## Security model

Factor is a personal agent, not a multi-tenant service. The guardrails (workspace
restriction, exec deny-patterns, allowlists, secret redaction) protect against
accidents and casual prompt-injection — they are **not** a security boundary. Run it
under your own account for yourself; set `channels.telegram.allow_from`; keep
`restrict_to_workspace` on unless you know why you're turning it off.

## Development

```bash
make check        # gofmt + vet + race tests
make build        # local binary
make build-all    # release cross-compile (incl. GOAMD64=v1 for old x86-64)
make build-tiny   # -tags nobrowser: smallest binary
```

The test suite runs against fakes (scripted providers, fake smrti, fake Telegram
API, a re-exec fake MCP server, a scripted desktop where every helper command is
asserted) plus live tests — a real headless-Chrome browser run and a real
desktop round-trip — that auto-skip when the machine cannot host them.

## License

MIT © Cyqle Labs
