# ADR-002: Cache the prompt prefix explicitly

## Status

Accepted

## Date

2026-08-31

## Context

`Loop.assemble` (`internal/agent/loop.go`) has always ordered a request so that
two consecutive turns of a session share the longest possible byte-identical
prefix:

```
[system]  systemPrompt        — invariant by construction
[system]  rolling summary     — changes only when compaction runs
          prior history       — append-only; masking is monotone
[user]    turnContext         — recall, clock, briefing: varies every turn
          current turn
```

That shape cost real design effort and constrains the code in ways that look
arbitrary from the outside. `ContextBuilder.SystemPrompt` takes no arguments so
it cannot vary. `TurnContext` had to become a trailing **user** message rather
than a system one, because the Anthropic dialect hoists every system message to
the head of the request and would have silently undone the ordering. The
speaker's name is marked on the message rather than in the prompt head for the
same reason.

All of that exists to be rewarded by a prompt cache. On the OpenAI-compatible
dialects it is, automatically. On the native Anthropic dialect it was not:
caching there is opt-in per request, and `internal/provider/anthropic.go` sent
no `cache_control` anywhere. Factor was paying the entire architectural price
of prefix stability and collecting none of the return on the one provider where
the return has to be asked for.

Two smaller consequences followed from the same gap. `provider.Usage` carried
only prompt and completion tokens, so `internal/cost` priced every prompt token
at the full input rate — over-reporting spend on any provider that caches, and
over-reporting it more the better the prefix hygiene got. And there was no
signal at all for prefix stability: everything that keeps the prefix identical
fails silently, so a clock creeping into the system prompt, a tool list that
stopped sorting, or a recall string moved one message earlier would all have
cost every turn of every long session with nothing to show for it.

## Decision

**Mark the boundaries in the neutral layer, place the breakpoints in the
dialect.** `provider.Message` gains `CacheMark`, a hint that this message ends
a stretch the next request is likely to repeat. It is set by `assemble`, which
is the only place that knows the assembly order, and it is never persisted
(`json:"-"`): it describes one request, not the conversation.

Marks go in three places:

- **every system message.** The prompt is invariant and the summary under it is
  not, so a compaction that rewrites the summary costs the summary's entry and
  not the tool schemas and system prompt rendered in front of it.
- **the end of prior history**, which is where two consecutive turns of a
  session first differ. Marking after `turnContext` would write an entry over
  bytes nothing reads back.
- **the tail, before every model call** (`markTail`). The growing half of a
  turn — up to twenty iterations of tool calls and their results — was
  reprocessed from scratch on every one of those iterations.

The Anthropic adapter turns marks into `cache_control` and thins them to the
four the API accepts, keeping the first (the fixed head, which every later
request re-reads whole) and the most recent (where a growing request diverges).
System messages become one content block each rather than one merged string,
which is what lets a breakpoint sit between the prompt and the summary.

**Count what the cache did.** `provider.Usage` gains `CacheReadTokens` and
`CacheWriteTokens` as subsets of `PromptTokens`, normalized across two dialects
that disagree about what "prompt tokens" means: Anthropic reports only the
uncached remainder as input, so the adapter adds the rest back. `internal/cost`
prices reads at a tenth and writes at a premium, and the ledger carries the
cached count so the usage report can show a hit rate.

## Consequences

The hit rate is the point of the last paragraph as much as the money is. It is
the only alarm Factor has for a prefix that stopped being stable — a number
that drops when something upstream broke silently — and `internal/evals` now
also asserts the invariants directly: the system prompt is byte-identical
across turns and sessions, the turn context is a user message, and the marks
land where this ADR says they do.

Two properties are now load-bearing and were not written down before:

- **`maskOldToolResults` is monotone.** Once a result is masked it stays
  masked, and the stub is stable. That is what lets masking and prefix caching
  coexist: the divergence point between consecutive turns sits about four tool
  results from the end rather than at the head. A change making masking depend
  on total history length would move it to the front and quietly cost every
  turn a full re-prefill.
- **A breakpoint searches back a bounded number of content blocks** for an
  earlier entry. `MaxToolIterations` defaults to 20 and each iteration appends
  an assistant message plus at least one tool result, so a turn adds far more
  than that window; `markTail` keeping pace is what stops a busy turn silently
  finding nothing. This is the failure mode that makes naive agent-loop caching
  look like it does not work.

The trade accepted: a cache write costs more than a plain read of the same
tokens, so a session of exactly one turn is marginally more expensive than
before. Break-even is the second turn, and a conversation is not one turn.
