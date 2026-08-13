# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Conventions

Commit messages follow [Conventional Commits v1.0.0](https://www.conventionalcommits.org/en/v1.0.0/) (`feat:`, `fix:`, `docs:`, `test:`, `chore:`, …).

## Commands

```bash
make check                                  # full gate: vet + race tests + coverage ≥90% + gofmt check (what CI runs)
go test ./internal/agent -run TestName      # single test
make test-race                              # race detector (needs CGO_ENABLED=1; everything else is CGO-free)
make cover                                  # statement coverage, fails under 90%
make lint                                   # golangci-lint (.golangci.yml)
make build                                  # local binary
make build-tiny                             # -tags nobrowser strips the CDP browser suite (stubs in internal/browser/browser_stub.go)
```

The coverage gate is real: new code needs tests or `make check` fails. Live tests (real headless Chrome, real desktop round-trip) auto-skip in `-short` mode or when the machine can't host them; the rest of the suite runs against fakes — scripted providers, a fake smrti sidecar (spawned by re-execing the test binary), a fake Telegram API, a fake MCP server over real stdio JSON-RPC, and a scripted desktop backend.

## Architecture

Factor is a single static Go binary (`cmd/factor`): a desktop AI agent with long-term memory, reachable over CLI or Telegram. `internal/app` is the composition root — it wires everything below into one `App` shared by the CLI modes and the `factor gateway` daemon (`internal/gateway`, which adds PID-file management, cron, and heartbeat).

Message flow:

```
channels → bus → agent loop → provider chain
                     └→ tool registry ─ every tool suite
memory recall injects into each turn's context; turns are stored back after
jobs / cron / heartbeat publish results onto the bus, re-entering sessions proactively
```

- **`internal/bus`** — bounded queues decoupling connectors from the loop. Publishing never blocks; a full queue drops loudly.
- **`internal/channel`** — connectors self-register via `channel.Register(name, factory)` in package `init()`, keyed to their own raw-JSON config section. Telegram is the model implementation.
- **`internal/agent`** — the loop enforces **one live turn per session key** (`channel:chatID`). A second message arriving mid-turn becomes *steering*: injected between tool iterations under the same lock that guards turn release, and leftovers are republished as fresh inbound. `compact.go` handles context-overflow compaction at turn-safe boundaries. `context.go` (ContextBuilder) assembles system prompt + skills catalog + memory recall.
- **`internal/provider`** — a failover `Chain` of candidates. `openai.go` speaks every OpenAI-compatible dialect (OpenRouter, Ollama, Groq, …); `anthropic.go` is native. `classify.go` decides retry vs. failover vs. compaction; `reasoning.go` translates one reasoning config into each provider's dialect.
- **`internal/memory`** — smrti (Python) runs as a supervised REST sidecar: spawned, health-checked, restarted with backoff, auto-installed when missing (`install.go`). `Ambient` does per-turn recall→inject and store; everything degrades gracefully (empty recalls, dropped writes) when the sidecar is down. Memory is also exposed as deliberate tools (`remember`/`recall`/`forget`/`reflect`).
- **`internal/tools`** — the registry. A tool implements `Name/Description/Parameters/Execute` and registers once; users disable via `tools.disabled` config. `guard.go` (PathGuard) enforces workspace restriction with symlink resolution; exec has deny-patterns; secrets are scrubbed from every tool result at the registry layer. Other suites register into the same registry: `internal/browser` (CDP), `internal/desktop` (X11/Wayland/macOS/Windows via helper commands), `internal/jobs` (background jobs whose completions re-enter the session), `internal/cron`, `internal/mcp` (mounts external MCP servers' tools at runtime), `internal/skills` (markdown skills, catalog-in-prompt with full text on demand).
- **`internal/config`** — `~/.factor/config.json`, every key optional, `FACTOR_*` env overrides declared as struct tags. The agent's home is `~/.factor/workspace` (`AGENT.md`, `SOUL.md`, `USER.md`, `HEARTBEAT.md`, `instructions/`, `skills/`, `sessions/`, `cron/`).
- **`internal/tui`** — the interactive chat console (used by `cmd/factor`): owns the bottom rows of the terminal — a live activity line plus a prompt that stays editable while a turn runs — with replies landing above; falls back to plain lines without a TTY (pipes, `TERM=dumb`).
- **`internal/wizard`** — the `factor init` interactive terminal wizard (verifies each step live: provider completion, Telegram `getMe`, model list from the endpoint).

Design intent: guardrails (PathGuard, deny-patterns, allowlists, redaction) protect against accidents and casual prompt injection — they are explicitly **not** a security boundary. Factor is a single-user personal agent.
