# ADR-003: Keep a turn trace, and watch it for drift

## Status

Accepted

## Date

2026-08-31

## Context

Factor knew everything about a turn while it was happening and kept none of it
afterwards.

- `internal/agent/activity.go` emits phase changes to a single live watcher for
  the status line, and drops them.
- `internal/tools/registry.go` runs every tool and records only the ones that
  panicked or were truncated — no timing, no outcome.
- `internal/cost` sees every model call, knows the session it belongs to, and
  stores two integers into a ledger.

So "why was that turn slow", "why did it cost that", "what has started
failing", and "it answered as the wrong person" were all reconstructed from
memory rather than read. The voice subsystem is the counterexample and shows
what the rest was missing: its `voice heard` line carries the speaker, the
branch that named them, the similarity it was judged on and the session it ran
in, which turns "it answered as the wrong person" into a readable line.

Separately, the heartbeat had the right instinct and the wrong reach. It wakes
on a timer and spends a model call only when `HEARTBEAT.md` holds something
task-shaped, which is correct — nothing to do, nothing to spend. But that gate
can only notice what the user thought to write down in advance, which is never
the thing that actually went wrong.

## Decision

**`internal/trace` writes one JSONL record per turn** under `~/.factor/traces`:
the models that answered with their token and cache split, the tools that ran
with duration and outcome, the events that only exist in aggregate (failover,
overflow, steering), and how the turn ended. Priced spend with no turn open —
an idle compaction, an induction verdict — gets a record of its own, because
spend nobody asked for is exactly the spend worth being able to find.

It is written from the three seams that already existed rather than from new
instrumentation: `Loop.execute` opens and closes the turn, `runTools` times
each call, and `cost.Meter.OnCharge` reports the money, since the meter is the
only place that sees both the model that actually answered and the price it was
billed at.

**Scope is deliberately small.** This is a single-user agent on the user's own
machine. The trace is a local file with a retention limit and never leaves the
box. It records what a tool was *called* rather than what was *said to it*:
arguments hold the user's file paths, their searches, and the things they asked
to be remembered, and the questions a trace exists to answer need the shape of
a turn rather than its contents. `trace.record_args` opts in and scrubs through
the same secret filter tool results pass. There is no OpenTelemetry export by
default; exporting a personal agent's trajectory off-box would be a privacy
regression, not an observability win.

**`internal/bands` reads those records** and reports which of Factor's own
numbers has left its band: tool error rate, turn failures, cost and seconds per
turn, provider failovers, context overflows, and the cache hit rate — the one
metric where a *fall* is the problem, because it is the only signal that the
request prefix stopped being byte-stable.

Detection is arithmetic: mean and standard deviation over a rolling baseline
against the last hour, with a direction per metric. **No model is involved in
it.** A flat baseline or too few samples says nothing rather than everything.

**The heartbeat is where it lands.** A breach starts a check even when
`HEARTBEAT.md` is empty, with the numbers stated and the conclusion left open.
The model is spent on judging whether an unusual reading matters — which is
exactly the judgement it is good for — instead of on confirming that nothing
happened.

## Consequences

The trace is also the corpus for two things that had none. Skill induction now
prefers a turn the user *corrected* over one that merely took a long time:
steering is the cheapest feedback this system gets and the only kind nobody has
to be asked for, and a trajectory holding both the approach that was wrong and
the one that worked is most of what makes a workflow worth writing down. And
`internal/evals` has real trajectories to build cases from rather than
imagined ones.

`Loop.NoteFeedback` is the seam for a connector reporting a correction the loop
cannot see. The loop already recognizes the two that reach it directly — a
message steered into a live turn, and a turn the user cancelled, which on voice
is a barge-in and lands as an interrupted outcome.

What this deliberately does not do: act. Every tier ends in a log line, a
sentence in the heartbeat's prompt, or the user's attention. A single-user
agent that rolled itself back on a statistical reading would be a worse
outcome than one that said something.
