# ADR-001: Statistical crusher for oversized tool results

## Status

Proposed

## Date

2026-08-19

## Context

A tool result enters the model's context verbatim and rides the next four tool
calls whole. After that `maskOldToolResults` (`internal/agent/mask.go:43`)
replaces it with a stub naming the call, recoverable only by calling again, and
compaction drops tool results from its summary transcript altogether
(`internal/agent/compact.go:256-270`). So what a result carries while it is
whole is what the model gets from it — and the cost argument this ADR opened
with, every result re-sent on every turn until compaction, no longer holds.

A registry-layer cap now bounds every tool alike, MCP servers included, and
names what it withheld and how to ask for less
(`internal/tools/registry.go:129-165`). The MCP suite's own cap was removed
when that landed. Underneath it, the tools that shape their own output keep
theirs:

| Tool | Cap | Where |
|---|---|---|
| every tool, at the registry | 16 000 chars, head + a recovery line | `internal/tools/registry.go:134-165` |
| `exec` | 32 KB, head+tail | `internal/tools/exec.go:41,111-114` |
| `read_file` | 256 KB | `internal/tools/fs.go:13` |
| `web_fetch` | 20 000 chars | `internal/tools/web.go:67` |
| `clipboard` | 100 000 chars | `internal/desktop/tools.go:437` |

The caps bound cost and the marker names the recovery path, but the cut is
still blind: the registry keeps the head and drops the rest. On a structured
result that is the worst possible cut — a 40 KB JSON array of 200 items keeps
the first eighty and loses everything after, including, possibly, the one
error object the model called the tool to find. `browser_read` already solved
this for its own output (rank what matters, state what was withheld, name the
recovery path — `internal/browser/browser.go:667-694,914-917`); nothing does
it for the tools whose output Factor does not shape.

The idea comes from analyzing [headroom](https://github.com/headroomlabs-ai/headroom),
whose SmartCrusher compresses JSON tool output 60–95% with a deterministic
statistical pass — no model call. Its full implementation is a ~465 KB Rust
crate; we take the idea, not the code, and not the Python dependency.

## Decision

Add a pure-Go, size-triggered crusher for tool results, applied at the
registry layer, with the size policy moved there too.

**The hook.** A third injected func in `NewRegistry` beside `enabled` and
`filter` (`internal/tools/registry.go:22-26`) — the existing style for
cross-cutting policy: nil-tolerant, defaulted, testable as a closure. It takes
`capResult`'s place in `Execute`'s deferred block (`registry.go:107`), and
runs *inside* `r.filter` rather than outside it as the cap does today, so the
statistics the crusher writes are redacted along with everything else.

**What it does.** When `ForLLM` exceeds the trigger threshold and parses as
JSON containing a large array: keep the first and last k items, plus every
item that looks like an error, carries a rare status value, or is a
structural outlier; replace the crushed middle with per-field statistics
(counts, ranges, distinct values); end with a line in the `browser_read`
mold — how many items existed, how many were shown, and what call gets the
rest (re-run with narrower arguments). Oversized results that are not
crushable JSON fall through to the cap that is already there, which states
what it withheld and how to narrow the call.

**The policy move.** Most of it has since happened on its own: the MCP cap is
gone and the registry's cap is already the effective bound. What is left is
`exec`, whose 32 KB head+tail reaches the registry pre-destroyed — it rises to
a transport ceiling of 256 KB (matching `read_file`'s existing precedent) so
the crusher sees the real payload. A hard ceiling stays at the source so a
runaway result cannot force an unbounded parse.

**Defaults** (tunable, in `ToolsConfig`): the trigger is the cap that already
exists, 16 000 characters, with crushed output targeting ~8 KB. Below it
nothing changes; above it the crusher takes `capResult`'s place. Both knobs
live in `internal/config` with the other tool knobs — defaults in
`Defaults()`, zero-value guard in `normalize()`.

## Alternatives considered

### Adopt headroom itself (proxy mode)

- Pros: zero Go changes — each provider candidate has `api_base`, and the
  proxy speaks both dialects Factor uses. Works today as a user-side
  experiment, and `usage.json` measures the savings for free.
- Cons: a Python sidecar plus a HuggingFace model in the hot path of every
  turn. The memory sidecar degrades gracefully when down; the provider path
  cannot — a dead proxy is a mute agent. The ML wheels also cannot run on the
  weakest deployment target (no AVX/SSE4.2, 3.5 GB RAM).
- Rejected as a dependency. Pointing `api_base` at a headroom proxy remains
  available to any user without Factor's involvement.

### Port SmartCrusher at parity

- Rejected on effort: ~465 KB of Rust for behavior Factor needs a few hundred
  lines of. The heuristics that matter — keep errors, keep outliers, keep the
  edges, summarize the middle — are the small core.

### Reversible retrieval (headroom's CCR)

- Store the original result, embed a handle, add a retrieval tool.
- Rejected: `tools.Result` has no metadata field (`internal/tools/tool.go:23-32`),
  no content-addressed store exists anywhere, and the store brings TTL and
  cleanup semantics with it. Factor's existing recovery idiom — name the
  narrower call that reaches what was withheld — covers the need without new
  infrastructure, and is what `browser_read` and `read_file` already do.

### Do nothing

- The caps already bound spend, so the token argument alone is weak.
- Rejected because the cut is blind: head+tail on a JSON array is worse
  signal per token than errors + outliers + stats, and an error lost to the
  middle is a wrong answer, not a cheaper one.

## Consequences

- **Latency**: one JSON parse plus a linear pass, single-digit milliseconds,
  in-process, and only on oversized results. No sidecar, no network hop.
- **Cache**: crushing happens once, at execute time, so the persisted text is
  byte-stable across turns — no KV-cache risk, unlike a proxy that
  re-compresses in flight and must fight to keep prefixes identical.
- **Quality**: errors and outliers are guaranteed to survive truncation for
  the first time; the results it touches shrink roughly 2–4× while carrying
  more of what the model was looking for.
- **Coverage**: new code needs tests (`make check` gates at 90%).

## Implementation plan

1. **`internal/tools/crusher.go`** — array classifier, error/rare-status/
   outlier detection, k-split keep, per-field stats, output formatting with
   the withholding line. Deterministic; table-driven tests.
2. **Registry hook** — third func in `NewRegistry`, taking `capResult`'s place
   in the `Execute` defer and moving inside `r.filter` (`registry.go:107`).
   Tests mirror the filter trio:
   nil-crusher passthrough, crusher applied, crusher/filter ordering
   (`internal/tools/tools_test.go:46`, `contract_test.go:301-330`).
3. **Policy move** — raise the exec cap (`internal/tools/exec.go:41`) to the
   256 KB transport ceiling; the MCP cap is already gone. While there: exec's
   slice is byte-indexed and can split a rune (`exec.go:112-114`) — cut on
   runes as `internal/jobs/tools.go:125-135` already does.
4. **Config** — `ToolsConfig` fields, `Defaults()`, first `normalize()`
   guard for the section, and config tests.
5. **Verify** — `make check` and `make lint`; existing truncation tests
   (`fs_exec_test.go:435-442`, `mcp/manager_extra_test.go:255-265`) updated
   for the new ceiling and markers.

No new tool is added, so the hardcoded tool-set contract tests
(`contract_test.go:31-44,103-115`) stay untouched.

Estimated size: 400–700 lines including tests.
