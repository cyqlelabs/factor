# Review instructions

How to review a change to Factor, whether the reviewer is a person or an
agent. The point is a consistent review rather than a thorough one: the same
passes on every diff, findings ranked the same way, and a bar for "Important"
that does not move with whoever is reading.

## Passes

Run these, and tag each finding with the pass it came from.

**Bugs.** Logic errors, broken edge cases, subtle regressions. Concurrency is
the one to look hardest at: this codebase runs several turns at once, tools
inside a turn at once, and a gateway alongside a terminal writing the same
files. A finding here needs a concrete failure — inputs or an interleaving,
and what goes wrong — not a suspicion.

**Context economics.** Anything that changes what goes into a request. The
system prompt must not vary by turn, channel or session; per-turn content
belongs after the last cache breakpoint; tool results are the bulk of a long
session's bytes and the masking that bounds them has to stay monotone. A change
that quietly makes the prefix vary costs every turn of every long session and
nothing fails, so this pass exists to catch it by reading.

**Safety rails.** PathGuard, the exec deny-patterns, secret scrubbing, the
result cap, the budget check before a call rather than after. These are rails
and not a sandbox, which is exactly why a change that routes around one needs
to say so out loud.

**Compliance with the plan.** Does the diff do what the change said it would,
and does the documentation still describe the code? CLAUDE.md is read by every
session; a change that makes it wrong is worse than one that leaves it thin.

## What "Important" means here

Reserve it for findings that would break behaviour, leak data, spend money
nobody agreed to, or make a documented invariant untrue. Everything else is a
nit — naming, ordering, a comment that could be sharper.

A missing test for new behaviour is Important. The coverage gate will catch the
number; it will not catch a test that exercises a line without asserting
anything about it.

## Cap the nits

At most five per review, then a count. A review whose first twenty comments are
style is a review whose real finding gets scrolled past.

## Do not report

- Anything `make check` or `make lint` already enforces: gofmt, vet, the
  coverage floor, the linters in `.golangci.yml`. CI says it better and sooner.
- Comment density or prose style. The commentary here is deliberately dense and
  explains why rather than what; that is the house style, not an accident.
- Generated files: `docs/assets/*.png` (rebuilt by `make diagrams`).

## Two standing rules

**A failing test is never an infrastructure flake until it has been proven to
be one.** Re-run it once; if it fails again it is real. Never skip, disable or
quarantine a test to get a green tick.

**A behavioural change needs an eval, not just a unit test.** If a change
touches the system prompt, the operating rules, a tool description, or how a
request is assembled, `internal/evals` is where it gets checked — that suite is
the only thing standing between a prompt edit and production.
