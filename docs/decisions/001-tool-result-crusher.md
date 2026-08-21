# ADR-001: Statistical crusher for oversized tool results

## Status

Proposed

## Date

2026-08-19

## Context

A tool result enters the model's context verbatim and is re-sent on every
subsequent turn until compaction moves past it — and compaction then drops
tool results from its summary transcript entirely (`internal/agent/compact.go:178-192`),
so whatever the result carried into the live window is all the model ever
gets from it.

Every high-volume tool bounds that today with its own ad-hoc cap:

| Tool | Cap | Where |
|---|---|---|
| `exec` | 32 KB, head+tail | `internal/tools/exec.go:41,111-114` |
| MCP tools | 32 KB, head+tail | `internal/mcp/manager.go:165-169` |
| `read_file` | 256 KB | `internal/tools/fs.go:13` |
| `web_fetch` | 20 000 chars | `internal/tools/web.go:67` |
| `clipboard` | 100 000 chars | `internal/desktop/tools.go:437` |

The caps bound cost, but they cut blind: the middle of the result is gone,
unrecoverably, and on `exec` and MCP the marker does not even say how to get
it back. On a structured result that is the worst possible cut — a 40 KB JSON
array of 200 items keeps items 1–20 and 180–200 and loses everything between,
including, possibly, the one error object the model called the tool to find.
`browser_read` already solved this for its own output (rank what matters,
state what was withheld, name the recovery path — `internal/browser/browser.go:771-812`);
nothing does it for the tools whose output Factor does not shape.

The idea comes from analyzing [headroom](https://github.com/headroomlabs-ai/headroom),
whose SmartCrusher compresses JSON tool output 60–95% with a deterministic
statistical pass — no model call. Its full implementation is a ~465 KB Rust
crate; we take the idea, not the code, and not the Python dependency.

## Decision

Add a pure-Go, size-triggered crusher for tool results, applied at the
registry layer, with the size policy moved there too.

**The hook.** A third injected func in `NewRegistry` beside `enabled` and
`filter` (`internal/tools/registry.go:25`) — the existing style for
cross-cutting policy: nil-tolerant, defaulted, testable as a closure. It runs
in `Execute`'s deferred block (`registry.go:99-130`) *before* the secret
filter, so redaction is applied to the final text.

**What it does.** When `ForLLM` exceeds the trigger threshold and parses as
JSON containing a large array: keep the first and last k items, plus every
item that looks like an error, carries a rare status value, or is a
structural outlier; replace the crushed middle with per-field statistics
(counts, ranges, distinct values); end with a line in the `browser_read`
mold — how many items existed, how many were shown, and what call gets the
rest (re-run with narrower arguments). Oversized results that are not
crushable JSON keep head+tail, but the marker is upgraded to state the bytes
withheld and the recovery path.

**The policy move.** The MCP and `exec` caps rise to a transport ceiling of
256 KB (matching `read_file`'s existing precedent) so the crusher sees the
real payload instead of a pre-destroyed one; the registry's trigger becomes
the effective bound. A hard ceiling stays at the source so a runaway result
cannot force an unbounded parse.

**Defaults** (tunable, in `ToolsConfig`): trigger at 16 KB, crushed output
targeting ~8 KB. Both live in `internal/config` with the other tool knobs —
defaults in `Defaults()`, zero-value guard in `normalize()`.

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
2. **Registry hook** — third func in `NewRegistry`, applied at the top of the
   `Execute` defer, before `r.filter`. Tests mirror the filter trio:
   nil-crusher passthrough, crusher applied, crusher/filter ordering
   (`internal/tools/tools_test.go:46`, `contract_test.go:301-330`).
3. **Policy move** — raise the MCP cap (`internal/mcp/manager.go:165-169`)
   and the exec cap (`internal/tools/exec.go:41`) to the 256 KB transport
   ceiling. While there: exec's slice is byte-indexed and can split a rune —
   cut on runes as `internal/jobs/tools.go:127-131` already does.
4. **Config** — `ToolsConfig` fields, `Defaults()`, first `normalize()`
   guard for the section, and config tests.
5. **Verify** — `make check` and `make lint`; existing truncation tests
   (`fs_exec_test.go:435-442`, `mcp/manager_extra_test.go:255-265`) updated
   for the new ceiling and markers.

No new tool is added, so the hardcoded tool-set contract tests
(`contract_test.go:31-44,103-115`) stay untouched.

Estimated size: 400–700 lines including tests.
