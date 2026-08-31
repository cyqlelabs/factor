# Factor against the 2026 agent-lifecycle literature

A read of three documents against this codebase, and what they suggest changing.

| | |
|---|---|
| **ADLC** | *The Agent Development Lifecycle*, Harrison Chase, LangChain, 9 May 2026 |
| **Google** | *The New SDLC With Vibe Coding*, Osmani / Saboo / Kartakis, Google × Kaggle, May 2026 |
| **Anthropic** | *The AI-Native SDLC Playbook*, Louis Claxton, Anthropic Applied AI, 21 Aug 2026 |

Two of the three are about building software *with* agents. Factor is an agent, so
their advice arrives twice: once as guidance for the runtime Factor ships, once as
guidance for the repository that ships it. The runtime lens is where nearly all the
value is, and this document is weighted accordingly.

---

## 1. The claim all three make

Google states it as an equation — **Agent = Model + Harness** — and then makes it
falsifiable: on Terminal Bench 2.0 one team moved a coding agent from outside the
top 30 to the top 5 *by changing only the harness*, and LangChain raised a score on
the same benchmark by 13.7 points by tweaking only the system prompt, tools and
middleware around a fixed model. Their conclusion is blunt: "Most agent failures,
examined honestly, are configuration failures."

The harness is enumerated as instructions and rule files, tools, sandboxes,
orchestration logic, guardrails/hooks, and observability. That list is a fair
table of contents for `internal/`. Factor is not a consumer of this idea; it is
an instance of it. Which means the papers are useful here mainly where they name a
harness component Factor has not built yet — and there are four: **cache-aware
request assembly, model routing, a durable trace, and behavioural evals.**

The second shared claim is sharper and is the one worth internalising:

> Tests verify the deterministic parts of the system … Evals verify the parts that
> are not: did the agent take the right trajectory of steps, choose the right tools,
> and produce a final response that meets the quality bar. **Without both, the
> practice is always vibe coding, regardless of how sophisticated the prompts are.**
> — Google, p. 14

Factor has an unusually strong first half — a 90% coverage gate, race tests, a
private Xvfb driving real `xdotool` against real pixels — and no second half at
all. Section 6 takes that up.

---

## 2. What Factor already does that these papers describe as the destination

Worth stating plainly, because it changes what the rest of this document is for.
Several of Factor's existing mechanisms are ahead of what the papers describe, and
should not be disturbed by anything below.

**Context engineering as a financial lever.** Google devotes a section to arguing
that context engineering "is not just a technical skill — it is a financial
strategy," and stops at the level of "write a good AGENTS.md." Factor has gone
several steps further and *measured* it: `internal/agent/mask.go` records that tool
results are 79–91% of the bytes in a real session history, and masks everything
past the last four into restorable stubs. `compact.go` budgets against the whole
request — system prompt and tool schemas included — rather than the history alone.
`trimInFlight` catches the turn that grows past budget mid-flight. No paper here
describes anything this specific.

**Static vs. dynamic context, and progressive disclosure.** Google's central
context-engineering distinction, and its recommendation of Agent Skills as the
pattern that resolves it, is `internal/skills` already: catalog in the prompt, full
text on demand. Anthropic's "skills as institutional knowledge" play is the same
mechanism.

**Guardrails as harness components.** PathGuard's symlink-resolving workspace
restriction, exec deny-patterns, registry-layer secret scrubbing, and a uniform
result cap that applies to MCP servers on the same terms as built-ins — that is
Google's "Guardrails" row and Anthropic's "hooks as build-time guardrails" play,
implemented in the right place (the registry, so no tool can opt out).

**Cost governance.** ADLC names cost as the *first* governance challenge. Factor
already prices every call, bills it to the session whose tool context it carried,
merges rather than overwrites `usage.json` so two processes spending at once do not
erase each other, and — the part most systems get wrong — checks caps *before* the
call, so a budget is a line the agent stops at rather than one it discovers it has
crossed.

**Human-in-the-loop.** ADLC's HITL requirement and Anthropic's "hooks can ask" play
are `internal/tools/ask.go`, which additionally solves the problem neither paper
raises: *where* the question goes when the user is not at a terminal, and what
happens when nobody answers.

**A learning loop inside the runtime.** ADLC's flywheel — traces become datasets,
failures become metrics, learnings feed the next version — is an organisational
practice in all three papers. `internal/agent/induce.go` runs it automatically, per
session, on a metered budget, with a categorical parse and a cap on the learned
library. Whatever else is true, Factor is not behind on this idea.

---

## 3. Turn on the prompt cache — the single largest unclaimed win

**Status: a designed-for optimisation that is currently switched off on the native
Anthropic path, and unmeasured everywhere.**

`internal/agent/loop.go:749` assembles every request in this order, and the ordering
is not incidental — CLAUDE.md devotes a paragraph to why `SystemPrompt` takes no
arguments and why `TurnContext` had to become a trailing **user** message rather
than a system one:

```
[system]  systemPrompt        — invariant by construction
[system]  rolling summary     — changes only when compaction runs
          prior history       — append-only, masking is monotone
[user]    turnContext         — recall, clock, briefing: varies every turn
          current turn
```

That is a textbook four-segment cacheable prefix, and it cost real design effort to
get. On the OpenAI-compatible dialects it is rewarded automatically. **On the native
Anthropic dialect it is not**: caching there is opt-in per request, and
`internal/provider/anthropic.go` emits no `cache_control` anywhere — `anthReq.System`
is a plain string, and the tool array carries no marker. Factor is paying the entire
architectural price of prefix stability and collecting none of the return on the one
provider where the return has to be asked for.

The prize is not marginal. Cache reads bill at roughly **0.1×** base input; writes at
**1.25×** on the default 5-minute TTL. Break-even is two requests. For a session
whose fixed preamble is a system prompt plus every tool schema — CLAUDE.md already
calls that "a fixed five-figure cost" — the second turn onward is close to free on
the largest single block in the request, and time-to-first-token falls with it.

### What to do

1. **Add breakpoints, four of them, at the stability boundaries Factor already has.**
   The API allows exactly four per request and renders `tools → system → messages`,
   so a marker on the last system block caches tools and system together. Factor's
   four segments map onto the four slots with nothing left over.

2. **Mind the 20-block lookback.** A breakpoint walks backward at most 20 content
   blocks to find a prior entry. `Agent.MaxToolIterations` defaults to **20**
   (`internal/config/config.go:336`), and each iteration appends an assistant message
   plus at least one tool result — so a busy turn adds 40+ blocks and the next
   request's tail breakpoint silently finds nothing. This is the failure mode that
   makes naive agent-loop caching look like it does not work. The fix is an
   intermediate breakpoint roughly every 15 blocks within a long turn, which the loop
   is well placed to emit because it already re-assembles the request each iteration.

3. **Parse the cache counters into `provider.Usage`.** Today `provider.Usage`
   (`types.go:52`) carries `PromptTokens` and `CompletionTokens` and nothing else, and
   `cost.Meter.record` prices every prompt token at the full input rate. Both dialects
   report more than that — Anthropic as `cache_creation_input_tokens` /
   `cache_read_input_tokens`, the OpenAI dialects as
   `prompt_tokens_details.cached_tokens`. Two consequences follow, and the second
   matters more than the first:
   - **`usage.json` over-reports spend today**, on every provider that caches, by
     billing discounted tokens at full rate. The better Factor's prefix hygiene gets,
     the more the meter over-charges.
   - **Cache hit rate becomes a regression alarm for prefix stability.** Right now, a
     change that accidentally makes the prefix vary per turn — a clock that creeps
     into the system prompt, a tool list that stops sorting, a recall string that
     moves one message earlier — is completely invisible. With the counter in the
     ledger it is a number that drops. Given how much of Factor's design rests on
     prefix invariance, this is the missing test for an invariant the codebase
     otherwise defends only by comment.

4. **Pin the masking/caching interaction with a test.** `maskOldToolResults` is
   *monotone*: once a result is masked it stays masked, and `maskedResult` renders
   the same stub every time. That property is what lets masking and prefix caching
   coexist — the divergence point between consecutive turns sits about four tool
   results from the end, not at the head. It is load-bearing and undocumented. A
   change to `keepRecentToolResults`' semantics that made masking depend on total
   history length would move the divergence to the front and quietly cost every turn
   a full re-prefill, with no test failing.

5. **Consider pre-warming at gateway start.** A `max_tokens: 0` request runs prefill,
   writes the cache at the breakpoint and returns immediately with no output tokens
   billed. Factor is a daemon that idles and then gets spoken to; paying one write at
   startup to take the cache miss off the user's first sentence is a good trade
   specifically for the voice and phone paths, where time-to-first-token *is* the
   product. Only worth it if traffic is bursty enough that the entry would be cold.

**One note for later, not now.** Factor routes `turnContext` through a user message
because the Anthropic dialect hoists system messages to the head and would undo the
placement. Recent Anthropic models accept **mid-conversation system messages**
appended to `messages[]` — which is the properly-typed version of exactly this
workaround, and is described as the injection-safe operator channel. It is
model-dependent (Opus 5 / 4.8 / Fable / Mythos, not Sonnet 5), so the user-message
framing must stay as the portable default; this would be a per-model refinement, and
the current approach is not wrong.

---

## 4. Route the housekeeping calls to a cheaper model

**Status: not implemented; the seam already exists.**

Google is direct about this:

> In a vibe coding workflow, a developer typically relies on a single, massive
> frontier model for every interaction — paying premium token prices just to ask the
> AI to fix a typo … A well-designed factory model avoids this waste. It uses large,
> advanced models for highly complex tasks … but automatically routes deterministic,
> lower-complexity tasks … to smaller, faster, and significantly cheaper models.

Factor has exactly four provider call sites, and all four share `l.chat`:

| Site | Work | Suits a frontier model? |
|---|---|---|
| `loop.go:531` | the turn itself | yes |
| `loop.go:691` | `wrapUp` — final answer, tools withheld | yes |
| `compact.go:284` | the compaction summary | no — summarisation |
| `induce.go:184` | SKIP/LEARN on a rendered trajectory | no — binary classification |

The induction call is the clearest case: a two-way classification over a bounded
trajectory, billed to the user's session at frontier prices, fired on every session
that goes quiet. Compaction is summarisation of text the model has already seen.
Neither is work the top of the range is for.

The seam is already half-built — `provider.Request.NoReasoning` exists precisely to
mark "a housekeeping call — a summary, a classification — whose budget has to reach
the answer," so the *concept* of a second class of call is already in the type
system. What is missing is letting that class resolve to a different chain. A
`provider.utility` candidate list in config, falling back to the main chain when
unset, is a small change with three payoffs: lower spend on calls the user never
sees, faster idle sweeps, and less risk that a compaction summary is what pushes a
session into its budget cap.

One caveat the papers do not mention and Factor should: routing costs a cache
namespace. Caches are model-scoped, so a compaction summary generated on a different
model does not share the conversation's cached prefix. That is fine here — the
compaction call re-reads history that is about to be replaced anyway — but it is a
reason not to extend routing to anything in the main turn path.

---

## 5. Run independent tool calls concurrently

**Status: strictly sequential.**

`loop.go:598` walks `resp.ToolCalls` one at a time. The recent de-duplication work
(`served`, commit dc36454) means one batch never runs the same call twice, but two
*different* calls in one batch still run back to back. Models emit batched calls
routinely — read three files, search two ways, check a page and a memory — and
essentially every Factor tool is I/O-bound: browser round-trips over CDP, HTTP,
the smrti sidecar, `exec`.

The Anthropic API's own guidance on parallel tool use is to execute the blocks
concurrently and return all results in a single user message, which is what the loop
already does structurally; only the execution is serialised.

This cannot be a blanket change. Desktop input is order-dependent by definition — a
`mouse` and a `key` in one batch mean something in sequence and nothing in
parallel — and browser navigation is stateful. So it needs an opt-in capability on
the `Tool` interface (a `Parallel() bool`, or a marker interface in the registry),
defaulting to off, with concurrency only when *every* call in the batch declares it.
The obvious first opt-ins are the read-only ones: file reads, `recall`,
`browser_fetch`, `skill_find`.

The payoff is wall-clock latency on exactly the turns where Factor's "never keeps
you waiting" promise is under the most pressure — a voice turn spending four
sequential browser reads is four round-trips of silence that the `PhaseNotice`
mechanism can describe but not shorten.

---

## 6. A trace per turn — the primitive the ADLC is built on

**Status: nothing durable.**

ADLC is the most insistent of the three here, and its argument is structural rather
than aspirational:

> A trace captures the full trajectory of the agent: the inputs it received, the
> model calls it made, the tools it invoked, the outputs it received, and the final
> response or action it produced. … If you cannot see the trajectory, you cannot
> reliably debug the behavior or turn those failures into future evals.

Google lists observability as a harness component and adds: "Without observability,
there is no way to tell whether the agent is doing well or quietly drifting."

Factor has the *ingredients* and keeps none of them. `internal/agent/activity.go`
emits phase changes to a single live watcher for the TUI and drops them.
`internal/tools/registry.go:100` runs every tool and records no timing, no argument
digest, no outcome — only panics and truncations reach the log. `cost.Meter.Chat`
sees every model call, already knows the session key, and stores two integers.

**Proposal: one JSONL span file per turn under `~/.factor/traces/`**, written from
the three seams that already exist. Per turn: id, session key, channel, trigger
(user / cron / heartbeat / job), the models that actually answered, per-iteration
tool calls with duration and result size and error flag, tokens in/out/cached, cost,
phase timings, and the events that are currently invisible in aggregate — failover,
compaction, steering, budget refusal, overflow recovery.

Three things this buys that nothing else does:

- **Debuggability.** "It answered as the wrong person" and "it went quiet for ninety
  seconds" become readable rather than reconstructed. Factor's voice logging already
  demonstrates the value of this discipline — the `voice heard` line carrying the
  speaker, the branch that named them, the similarity and the session is exactly the
  right instinct, applied to one subsystem. This generalises it.
- **The eval corpus.** Section 7 needs hard examples, and ADLC is emphatic that the
  best ones come from real traces rather than imagination.
- **The audit trail ADLC asks for under governance** — which agent called which tool,
  with what inputs, producing what. Factor's security model is explicitly "rails, not
  a sandbox," and that is a defensible position for a single-user agent. It is more
  defensible with an after-the-fact record: rails you cannot inspect are rails you
  cannot tune.

Keep it proportionate. This is a personal agent, not an enterprise telemetry
pipeline: local files, rotated and capped, scrubbed through the same secret filter
tool results already pass, off or trivially disableable. An OTel exporter is a
reasonable optional extra for anyone who wants one, and should not be the default.

---

## 7. Evals for the agent, distinct from tests for the code

**Status: absent. This is the largest genuine gap.**

Anthropic's play is concrete: collect 20–50 real tasks with their accepted outcomes,
write each as a prompt plus the checks that define acceptable, run them
non-interactively in CI **on any change to the files that steer the agent**, gate
configuration changes on the pass rate, and turn every incident into a permanent
regression case.

Factor's test suite is genuinely excellent at what it covers, and covers none of
this. The things that actually steer Factor's behaviour — `SystemPrompt`,
`operatingRules`, `channelBriefing`, the rules restated past `rulesFadeAt`, every
tool `Description()`, the induction prompt, the compaction prompt — are the least
tested surface in the repository. A prompt edit today ships on judgement alone. That
is precisely the state Google labels vibe coding "regardless of how sophisticated the
prompts are."

**The pattern is already in the repo.** `internal/agent/vision_loop_e2e_test.go` runs
the whole turn against a fake vision model over a local HTTP server, decodes the PNG
that actually crossed the wire, and asserts on what the model did. That is an eval
wearing a test's clothing. Generalising it costs far less than starting from nothing.

**Tier 1 — scripted, deterministic, in `make check`.** A fake provider driven by a
per-case policy, the real loop, the real registry. Assert on *trajectory*, which is
where these papers say the signal is:

- slow work is handed to `job_start` rather than run inline
- a web question reaches for the browser rather than answering from memory
- a shared-room turn recalls only from the shared space, and never writes into the private one
- a message arriving mid-turn lands between iterations, and one addressed to a wider audience does not
- masking leaves the last four results whole and stubs the rest
- a budget refusal comes back as a sentence, not an error
- a turn past `rulesFadeAt` still carries the restated rules

Every one of those is a documented invariant. None is currently checked end to end,
and all are deterministic, key-free, and fast.

**Tier 2 — a real model, behind a build tag, on a schedule.** For the genuinely
non-deterministic questions: does the persona hold across a long session, is a
compaction summary faithful to what it replaced, does induction produce a skill worth
keeping. Not per-PR; the papers are clear that eval suites are living things, and a
nightly is the right cadence for a personal project.

**Wire the trigger where Anthropic puts it** — a `paths:` filter in `ci.yml` covering
`internal/agent/context.go`, `internal/skills/**`, and the tool description surface.
A change to the prompt is a change to the product, and should be gated like one.

---

## 8. Give the heartbeat deterministic control bands

Anthropic's Stage 6 is the closest thing in these papers to something Factor already
has, and the difference is instructive:

> The script is version controlled and unit tested, and **detection stays entirely
> deterministic, with no model involved.** … At 1σ the script only logs, at 2σ it
> invokes Claude read-only to diagnose, and at 3σ Claude may act.

`internal/heartbeat` inverts this: it wakes on a timer and spends a model call to
decide whether anything needs attention, with `HasActionable` as a coarse
prose-level gate in front. The instinct is right — "no actionable content → no LLM
call" is the same instinct — but the gate can only read `HEARTBEAT.md`, so it can
only notice what the user thought to write down.

Once section 6 exists, Factor emits a stream of numbers about itself that nobody
watches: cost per turn, provider failover rate and cooldown depth, tool error rate by
tool, sidecar restarts, compaction frequency, budget proximity, the mic
digital-silence flag, room state. A deterministic watcher over the trace log with
tiered responses — log / diagnose read-only / surface to the user in the chat it can
reach — makes the heartbeat both **cheaper** (it stops paying a model to notice
nothing changed) and **sharper** (it notices "failover rate tripled since the config
change" and "this session is at 80% of its cap", which no `HEARTBEAT.md` would ever
say).

This is also the one place Factor could genuinely close Anthropic's loop rather than
approximate it. Factor already has `config_set`, `upgrade`, cron and jobs. A breached
band that writes its diagnosis and evidence into the workspace as a proposal, and
asks — through `ask_user`, which already knows where the user is — is the
personal-agent form of "the finding re-enters the pipeline as intent.md."

---

## 9. Store feedback beside the trace

ADLC: "It is not enough to store traces alone. Teams also need to store feedback with
those traces."

Factor has a richer free feedback signal than most agents and currently discards all
of it. A **barge-in** on voice is a user interrupting an answer they did not want. A
**steering message** mid-turn is a correction delivered in flight. A **`forget` call**
is a rejected memory. A **repeated question** is a failed answer. These are already
first-class events with dedicated code paths; none is recorded as a judgement on the
turn it interrupted.

Tagging them onto the turn's trace costs almost nothing and immediately improves
something that already exists: `induce.go` currently selects its candidate on "four or
more tool calls," a proxy for *effort*. Turns the user corrected are a proxy for
*difficulty* — and ADLC is explicit that "the most valuable eval datasets are built
from the hardest examples." The same sweep, the same metered call, a better sample.

---

## 10. Version the workspace

ADLC's Context Hub argument:

> Some of the most important parts of an agent are not traditional application code.
> Prompts, retrieval context, skills, and task instructions may need to change more
> often than the application itself. … That creates the need for a prompt or context
> hub: a place to store, version, review, and update the non-code parts of the agent.

Factor's shipped prompt is compiled into the binary and versioned with it, which is
correct. The *user's* half — `AGENTS.md`, `SOUL.md`, `USER.md`, `HEARTBEAT.md`,
`instructions/`, `skills/` — lives in `~/.factor/workspace` with no history at all.
Someone who tunes their agent's persona over six months has no diff, no blame, and no
rollback.

A `git init` on the workspace (opt-in, or on first run), with a commit on every write
through `skill_write`, `config_set` and induction, is cheap and buys the audit trail
the Anthropic playbook builds its entire lifecycle on: who changed what, when, and
what it looked like before. It also makes induction materially safer — a learned skill
that turns out to be bad is currently edited or capped out of existence, and would
instead be revertible.

---

## 11. The repository's own lifecycle

Smaller, because Factor is already well-run: Conventional Commits, a lint hook via
`make hooks`, a real CI gate that refuses to go green when the desktop helpers fail to
install, and a CLAUDE.md far past what Anthropic's play asks for.

Two honest observations:

- **The playbook's artifact chain is mostly already here, in an unusual form.** Factor's
  inline commentary carries more design rationale than most ADR directories do — the
  masking rationale, the room-confidentiality argument, the tray icon's export race,
  the speaker thresholds and the recordings they were measured against. `docs/decisions/`
  has one file. The gap is not that the reasoning was never written down; it is that
  it is distributed through the code and unaddressable from outside. Extraction, not
  authorship.
- **`REVIEW.md` is a free win.** Anthropic's play — passes to run, what "Important"
  means versus a nit, a nit cap, what to skip — costs one file and makes every future
  agent-assisted review on this repo consistent. There is nothing to build.

Anthropic's rule *"when Claude makes a mistake twice, the correction goes into
CLAUDE.md"* and *"keep it under a page"* are worth weighing against each other here:
this CLAUDE.md is very long, and long enough that its own advice about `rulesFadeAt`
applies to it. Some of it is architecture that belongs in `docs/`.

---

## 12. What not to take from these papers

They are written for enterprises, and a good deal of what they recommend would be
actively wrong here. Recording it so it does not get adopted by momentum:

- **Separation of duties, approval gates, release managers, change boards.** Factor is
  a single-user personal agent. The user *is* the code owner, the release manager and
  the security team. Anthropic's managed-settings block — `allowManagedHooksOnly`,
  `disableBypassPermissionsMode` — exists to stop an engineer from widening their own
  permissions. There is nobody to stop.
- **OTel export to an organisation's observability stack, the analytics dashboard, the
  Compliance API.** The trace in section 6 should be a local file. Exporting a personal
  agent's trajectory off-box by default would be a privacy regression, not an
  observability win — and this is an agent that listens to a room and knows who is in it.
- **Sandboxes as a hard boundary.** Google and ADLC both recommend them. Factor's
  position — "guardrails … are explicitly not a security boundary" — is a deliberate,
  documented choice appropriate to a tool that drives *your* desktop and *your* logged-in
  browser. Adopting sandbox language without the sandbox would be worse than the honest
  current framing.
- **Discoverability and reuse across teams.** ADLC's third governance challenge
  presupposes many teams. `skill_find` against skills.sh already covers the single-user
  version of it.

---

## Ranking

Ordered by value per unit of work. The first three are self-contained; 6 unlocks 7, 8
and 9.

| # | Change | Effort | Payoff | Confidence |
|---|---|---|---|---|
| 3 | `cache_control` breakpoints + cached-token accounting | S | Large cost and latency cut; makes prefix stability testable | High — the design already assumes it |
| 4 | Utility model for compaction and induction | S | Direct cut to spend the user never sees | High |
| 5 | Concurrent execution of parallel-safe tool calls | M | Wall-clock latency where it is felt most | High |
| 6 | Durable per-turn trace | M | Debuggability, audit trail, eval corpus | High |
| 7 | Eval suite, tier 1 scripted | M–L | The missing half of verification | High |
| 8 | Deterministic control bands for the heartbeat | M | Cheaper idle, sharper alerts | Medium — depends on 6 |
| 9 | Feedback signals tagged onto traces | S | Better induction candidates, real eval cases | Medium — depends on 6 |
| 10 | Versioned workspace | S | History and rollback for user-owned context | Medium |
| 11 | `REVIEW.md`, ADR extraction, eval CI trigger | S | Process consistency | High |

Nothing here argues with a design decision the codebase has already made. Items 3
and 5 are unclaimed returns on work that is already done; 4, 6 and 7 are harness
components the papers name and Factor has not built yet.
