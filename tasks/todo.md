# Factor — Task List

## Phase 1: Foundation
- [x] 1. Repo bootstrap (go.mod, .gitignore, LICENSE, Makefile, version, CI skeleton)
- [x] 2. Config + workspace (JSON + FACTOR_* env, templates, redaction, raw channel/MCP sections) + tests
- [x] 3. Providers (OpenAI-compat, Anthropic, classifier, fallback/cooldown) + tests
- [x] CHECKPOINT: build/vet/test clean

## Phase 2: Core chat vertical
- [x] 4. Bus + JSONL session store + tests
- [x] 5. Agent loop (steering, turn state machine, context builder + instructions.d, tool registry w/ config gating, fs tools, CLI REPL) + e2e mock test
- [x] 6. exec + web_fetch + web_search tools + tests
- [x] CHECKPOINT: chat with tool use works; race clean

## Phase 3: The soul — smrti
- [x] 7. memory.Engine + smrti client + sidecar manager + tests
- [x] 8. Ambient recall/store + memory tools + turn wiring + tests
- [x] CHECKPOINT: inject/store verified vs fake smrti; degraded mode tested

## Phase 4: Companion daemon + extensibility
- [x] 9. Channel registry + Telegram connector + manager + gateway daemon + tests
- [x] 9b. Background job engine (job_start exec|task, status/list/cancel, completion → proactive session notification) + tests
- [x] 10. Cron + heartbeat + tests
- [x] 11. Skills loader + prompt slot + skill_install + tests
- [x] 12. MCP stdio client + dynamic tool mounting + mcp_* tools + tests
- [x] 13. Self-management tools (config_get/config_set, pkg_install) + tests
- [x] 13b. Browser suite (chromedp: attach-or-launch, headed default, nobrowser tag) + tests
- [x] CHECKPOINT: gateway runs; all green

## Phase 5: Polish & ship
- [x] 14. Session compaction + tests
- [x] 15. README, Makefile build-all, CI + release workflows
- [x] 16. Full verification: lint, race tests, smoke, review
- [x] CHECKPOINT: complete

## Phase 6: First-run experience
- [x] 17. smrti auto-install (uv/pipx/pip --user/venv, PEP-668 retry, PATH-independent
      discovery) wired into `factor init` and the sidecar supervisor + tests
- [x] 18. `factor init` wizard (arrow-key menus, masked input, spinners; provider +
      live model list + verification, reasoning effort/budget, memory, Telegram,
      desktop helpers, tools) with a non-TTY/`-y` path + tests
- [x] 19. Desktop tool suite (window_list/window_control/screenshot/mouse/type_text/
      press_key/clipboard/notify/open/desktop_info) across X11, Wayland, macOS and
      Windows, registered by default when a display exists + tests
- [x] 20. Reasoning configuration translated per provider dialect (OpenRouter object,
      OpenAI/Groq reasoning_effort, Anthropic thinking budget), default xhigh + tests
- [x] CHECKPOINT: fmt/vet/race clean; wizard verified over a real pty; smrti
      auto-install verified against the real package
