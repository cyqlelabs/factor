# Factor — Task List

## Phase 1: Foundation
- [x] 1. Repo bootstrap (go.mod, .gitignore, LICENSE, Makefile, version, CI skeleton)
- [x] 2. Config + workspace (JSON + FACTOR_* env, templates, redaction, raw channel/MCP sections) + tests
- [x] 3. Providers (OpenAI-compat, Anthropic, classifier, fallback/cooldown) + tests
- [ ] CHECKPOINT: build/vet/test clean

## Phase 2: Core chat vertical
- [x] 4. Bus + JSONL session store + tests
- [x] 5. Agent loop (steering, turn state machine, context builder + instructions.d, tool registry w/ config gating, fs tools, CLI REPL) + e2e mock test
- [x] 6. exec + web_fetch + web_search tools + tests
- [ ] CHECKPOINT: chat with tool use works; race clean

## Phase 3: The soul — smrti
- [x] 7. memory.Engine + smrti client + sidecar manager + tests
- [x] 8. Ambient recall/store + memory tools + turn wiring + tests
- [ ] CHECKPOINT: inject/store verified vs fake smrti; degraded mode tested

## Phase 4: Companion daemon + extensibility
- [x] 9. Channel registry + Telegram connector + manager + gateway daemon + tests
- [x] 9b. Background job engine (job_start exec|task, status/list/cancel, completion → proactive session notification) + tests
- [x] 10. Cron + heartbeat + tests
- [x] 11. Skills loader + prompt slot + skill_install + tests
- [ ] 12. MCP stdio client + dynamic tool mounting + mcp_* tools + tests
- [ ] 13. Self-management tools (config_get/config_set, pkg_install) + tests
- [ ] 13b. Browser suite (chromedp: attach-or-launch, headed default, nobrowser tag) + tests
- [ ] CHECKPOINT: gateway runs; all green

## Phase 5: Polish & ship
- [x] 14. Session compaction + tests
- [ ] 15. README, Makefile build-all, CI + release workflows
- [ ] 16. Full verification: lint, race tests, smoke, review
- [ ] CHECKPOINT: complete
