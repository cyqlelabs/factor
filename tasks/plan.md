# Implementation Plan: Factor

## Overview

Factor is a lightweight desktop AI agent and companion in Go, distilled from PicoClaw's
architecture. It keeps PicoClaw's strongest ideas — message bus + bounded per-session
workers with mid-turn steering, narrow pluggable seams (`Provider`, `Tool`, `Channel`,
`memory.Engine`), provider failover with error classification, progressive-disclosure
markdown skills, sleep-not-tick schedulers, CGO-free static binaries — and replaces the
weakest subsystem (append-only `MEMORY.md` with no consolidation) with **smrti**
(github.com/cyqlelabs/smrti), an AtomSpace-inspired Bayesian memory engine, run as a
managed localhost REST sidecar. smrti is the soul of the agent: every exchange is stored
as episodes, every turn recalls salience-ranked memories (critical warnings, known
antipatterns, context) into the system prompt, and consolidation/forgetting happen
inside smrti's reflect epochs.

## Architecture Decisions

- **Go 1.25, CGO_ENABLED=0, `internal/` packages** — single static binary, cross-compiles
  to the low-resource Puppy Linux box (GOAMD64=v1 baseline, no SSE4.2 assumptions).
- **Tiny dependency budget**: `adhocore/gronx` (cron expressions), `caarlos0/env/v11`
  (env overrides), `golang.org/x/net/html` (web tool parsing). No SQLite in Go — storage
  belongs to smrti. Sessions are JSONL. REPL is stdlib. No cobra; stdlib `flag` subcommands.
- **smrti integration = REST sidecar, not MCP and not the proxy.** Factor spawns
  `smrti serve rest --host 127.0.0.1` with env (`SMRTI_DB`, tenant/space, personality,
  ignore patterns, extraction endpoint = Factor's own OpenAI-compatible provider, or
  `local` mode otherwise), health-checks `/status`, restarts with backoff, and degrades
  gracefully (empty recalls, dropped writes, health flag) when unavailable. `external`
  mode connects to an already-running smrti URL; `off` disables memory.
- **Native memory injection** (mirrors smrti's proxy semantics inside Factor so any
  provider works): recall query built from the last N messages; results injected as
  behavioral constraints (`critical_warning` → YOU MUST NOT, `known_antipattern` →
  AVOID) plus background notes, each with a confidence qualifier. Exchanges stored
  async post-turn; heartbeat traffic excluded on both client and server side.
- **Memory tools for the LLM**: `remember`, `recall`, `forget`, `reflect`,
  `memory_status` — deliberate memory on top of the ambient loop.
- **One turn per session, steering for overflow** — PicoClaw's best concurrency
  invariant, kept; control flow collapsed to a single explicit state enum
  (no Control/ToolControl flag spread).
- **Extensibility is a first-class constraint** (user requirement):
  - *Connectors*: self-registering channel factories (`channel.Register(name, factory)`);
    each connector is one package decoding its own raw-JSON config section. Telegram is
    the reference implementation; WhatsApp/Twilio/etc. slot in without touching core.
  - *Tools*: a global tool registry with config-driven enable/disable
    (`tools.disabled`), so the end user curates the arsenal; new tools register in one line.
  - *MCP*: built-in MCP client (JSON-RPC 2.0 over stdio); servers declared in config
    (`mcp.servers`) or added at runtime by the agent itself; their tools mount
    dynamically as `<server>__<tool>`.
  - *Instructions*: bootstrap files + `agent.extra_instructions` + drop-in
    `workspace/instructions/*.md` appended to the system prompt.
  - *Self-management tools*: `config_get`/`config_set` (redacted reads, hot-reload
    writes), `pkg_install` (detected package manager), `mcp_add`/`mcp_remove`/`mcp_list`,
    `skill_install` — the agent can extend itself.
- **Channels: CLI + Telegram** (raw HTTP long-poll, no SDK).
- **Safety**: workspace restriction for fs/exec, exec deny-pattern regexes (guardrail,
  documented as not a security boundary), sender allowlists, secret redaction of config
  values from tool output.
- **Sessions**: append-only JSONL + meta sidecar, summarize-compaction at turn-safe
  boundaries (never splits tool call/response pairs).

## Task List

### Phase 1: Foundation
- [ ] Task 1: Repo bootstrap — go.mod, .gitignore, LICENSE (MIT), Makefile, version pkg, CI skeleton (`go build ./...` green)
- [ ] Task 2: Config + workspace — JSON config at `~/.factor/config.json`, `FACTOR_*` env overrides, defaults, secret redaction; workspace ensure with AGENT.md/SOUL.md/USER.md/HEARTBEAT.md templates. Tests.
- [ ] Task 3: Provider layer — `Provider` interface, OpenAI-compatible + Anthropic impls, error classifier (auth/rate_limit/network/timeout/context_overflow/…), fallback chain with per-candidate cooldown. httptest tests.

### Checkpoint: Foundation
- [ ] `go build ./...`, `go vet ./...`, `go test ./...` clean

### Phase 2: Core chat vertical
- [ ] Task 4: Bus + JSONL session store (append-only history, meta sidecar, truncate/compact primitives). Tests.
- [ ] Task 5: Agent loop — session claim + steering queue, bounded workers, turn state machine (single enum), context builder (identity + bootstrap files + skills slot + memory slot, mtime cache), tool registry with JSON-schema arg validation and panic recovery; fs tools (read/write/edit/list); CLI channel (REPL + `-m` one-shot). End-to-end test with a scripted mock provider.
- [ ] Task 6: exec tool (deny patterns, workspace guard, timeout) + web_fetch + web_search (DuckDuckGo HTML, no key). Tests.

### Checkpoint: Core chat
- [ ] Interactive chat with tool use works against a mock provider in tests; race detector clean

### Phase 3: The soul — smrti
- [ ] Task 7: `memory.Engine` interface + smrti REST client + sidecar process manager (spawn, health, backoff restart, graceful stop, external/off modes). Tests against an httptest fake smrti.
- [ ] Task 8: Ambient memory — recall injection into the system prompt, async episode storage post-turn, ignore patterns; memory tools (remember/recall/forget/reflect/memory_status). Turn-loop wiring + tests.

### Checkpoint: Memory
- [ ] Agent turn injects recalled memories and stores the exchange (verified against fake smrti); degraded-mode behavior tested

### Phase 4: Companion daemon
- [ ] Task 9: Channel registry + Telegram connector (long-poll, chunked sends, allowlist) + channel manager + gateway daemon (PID singleton, signal handling, health endpoint). Tests.
- [ ] Task 10: Cron service (gronx, sleep-until-next-job, jobs.json) + `cron` tool; heartbeat (HEARTBEAT.md, skip-if-empty, HEARTBEAT_OK suppression, no-history turns). Tests.
- [ ] Task 11: Skills — loader (frontmatter, workspace/global roots), summaries-only prompt slot, progressive disclosure via read_file, `skill_install`. Tests.
- [ ] Task 12: MCP client (stdio JSON-RPC 2.0: initialize/tools list/tools call) + dynamic tool mounting + `mcp_add`/`mcp_remove`/`mcp_list` tools. Tests against a fake MCP server.
- [ ] Task 13: Self-management tools — `config_get`/`config_set` with redaction + hot reload, `pkg_install` (apt/apk/pacman/pip/npm detection). Tests.

### Checkpoint: Daemon
- [ ] `factor gateway` runs CLI-less with cron + heartbeat + Telegram wired; all tests green

### Phase 5: Polish & ship
- [ ] Task 14: Session compaction — token estimator, summarize threshold/percent, turn-boundary-safe cut, summary via provider. Tests.
- [ ] Task 15: README.md, Makefile build-all (linux amd64 GOAMD64=v1 / arm64 / 386, darwin arm64, windows amd64), CI (lint + vet + race tests + govulncheck + cross-build), release workflow (tag → GitHub Release binaries).
- [ ] Task 16: Full verification — golangci-lint, `go test -race ./...`, binary smoke test, code review pass.

### Checkpoint: Complete
- [ ] CI green locally (lint, vet, race tests), cross-builds succeed, README accurate

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| smrti sidecar not installed / Python absent | High | Explicit health status, graceful degradation, `factor status` surfaces it, README covers `pip install smrti` |
| smrti REST API drift | Med | Client covers only the 8 stable endpoints; contract tests against recorded shapes |
| Exec tool misuse | Med | Deny patterns + workspace guard + restrict-by-default; documented as guardrail not sandbox |
| Telegram API quirks (4096 limit, markdown errors) | Low | Chunking + plain-text fallback on parse errors |
| Context overflow on small local models | Med | Compaction + capped memory injection chars |

## Open Questions

- None blocking — module path assumed `github.com/cyqlelabs/factor` (smrti's org).
