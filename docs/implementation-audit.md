# Implementation audit

Audited: `50445adf76a85614f661c06d072917a82fce0df1`, 2026-09-09
Resolved: same day, in the commit that carries this file
Audience: maintainers and reviewers

This review compared [README.md](../README.md) and [CLAUDE.md](../CLAUDE.md) with
the implementation, using the passes in [REVIEW.md](../REVIEW.md). It covered
audience isolation, background work, spending, persistence, and message
delivery — a targeted review rather than a certification of every feature.

Every finding is fixed, and every failure the audit reproduced is now a test in
the suite. The table at the end maps each probe to the test that replaced it.

## Findings

### 1. Background work bypassed shared-room memory isolation — fixed

The docs say a turn other people can hear recalls only shared memory.
`SpacePolicy.Scope` handled `job`, `cron` and `system` before it looked at the
audience, so those turns read the private `main` space even when the turn was
explicitly shared. Job completions made the same trip from the other side: the
notification published into the originating session carried no audience at all,
and scheduled turns got an outlet channel without one.

The audience is now read before the channel, so a machine turn somebody can
hear is scoped to the room like any other, and refused recall where there is no
shared space to isolate into. It travels with the work as well: `jobs.Origin`
carries it, a task sub-turn runs under it (`Loop.ProcessDelegated`), and the
completion message is published with it.

That still leaves the room free to change while the work runs, which is the
half the audit called "withheld or recomposed". Factor recomposes. The loop
asks the channel who can hear a reply at the moment it dispatches one
(`Loop.SetAudience` → `Manager.Audience` → `Voice.Audience`) and takes the wider
of that and the message's own audience, so a job started alone and finished in
company is written under the shared scope rather than withheld after the fact.
Cron and the heartbeat resolve the same way against their outlet.

### 2. Declaring company did not isolate the running turn — fixed

The `room` tool updated the room and told the model only shared memory was
being recalled. The running turn kept its original context, which was assembled
for an empty room and held whatever private memory had been recalled into it.
The claim was false for the one turn it was made in.

Relabelling cannot take back what is already in a request, so the tool now ends
that turn where it stands (`Voice.rescopeTurn`) and the same utterance is asked
again under the shared session. Cancelling stops the speech too: a note the
turn was about to say never reaches the speakers. A turn that is already shared
is left alone, so nothing loops.

### 3. Global caps used stale totals, and concurrent writes lost charges — fixed

`Ledger.Snapshot` read process-local state, so a gateway approved calls against
a global cap a terminal had already met. The read-merge-write had no
cross-process lock and one shared temporary filename, so two writers could read
the same total and overwrite each other. And `Meter.check` compared prior spend
with the limit without accounting for calls already in flight.

A snapshot now re-reads the file whenever it has changed underneath, adding
whatever this process has spent but not yet written. The merge runs under a
lock file every process takes, through a scratch file named per process. Calls
in flight are reserved at the average price of a call so far.

A cap remains a stop line rather than a hard ceiling, and `Meter.check` says so
in full: the call that crosses the cap finishes and is billed, a call's price
is not known until it answers, the reservation is an estimate, and a model
nothing prices moves no cap at all.

### 4. Interrupted compaction hid surviving history — fixed

`Store.Compact` replaced the history file and then reset the skip offset. A
crash or a failed write between the two left the long file's offset applied to
the short one, which sliced the surviving turns off the front of a history that
was still whole on disk.

The meta sidecar is now the commit point and is written first, marked
`compacting`, with the shortened history staged beside the live file until the
rename lands. `recoverLocked` finishes an interrupted compaction before
anything reads or appends: the staged file is adopted if it is still there, and
the marker cleared if the rename already happened. A commit that cannot be
written changes nothing.

### 5. Background jobs reused persistent conversation identities — fixed

Every engine restarted numbering at `j1`, and the id is also the session key a
task sub-turn runs under. Two unrelated jobs a restart apart — or two Factor
processes sharing a workspace — inherited one history and one spending bucket
while `job_start` promised a fresh agent run.

Ids now carry a per-engine random token (`j1-3f9c`): unique across engines and
processes, still short enough to read back.

### 6. Background shell execution skipped the deny patterns — fixed

`runExec` launched the shell directly. A command a custom deny pattern blocked
in the foreground ran happily as a background job — and delegating slow work is
a workflow the operating rules encourage.

The patterns are now a `tools.CommandGuard` rather than a field on the exec
tool, and both shells check the same one. The job engine refuses in `Start`, so
every caller is covered. A task job's prompt is not a command and is not
checked against shell patterns.

### 7. Restart draining did not establish delivery — fixed

`settle` watched the queue length. A message the pump had picked up was already
invisible to that check while its send, its retries and its playback were still
ahead of it. The restart notice was also removed before publication, so a
refused publish swallowed the one line the user was waiting for.

`Manager.InFlight` counts what has left the queue and not yet finished being
delivered, and the reload waits on both counts. When the bounded wait ends with
work outstanding it says so, rather than leaving "reloaded" and "delivered"
indistinguishable afterwards. The restart notice is cleared only once the queue
has taken it; a note that outlives its publication is bounded by the 15-minute
TTL, so the worst case is being told twice instead of never.

### 8. Failed connectors stayed advertised as reachable — fixed

`Manager.Start` logged a startup failure and kept the connector; `Serves`
checked only map membership. A heartbeat, a cron result or a restart notice was
therefore addressed to a channel that would drop it, and reported as delivered.

The manager now records which connectors actually came up. `Serves` answers
from that, `Running` lists them, and `Failed` names the rest with what they
said — carried into the startup log, the tray rows and `/health`. `Names` still
answers what is configured, which is a different question.

## Further improvements

**Execution output is bounded during capture.** `ExecTool` buffered everything
before truncating, so a noisy command could exhaust memory to produce 32 KB.
Output now goes into a bounded buffer as it is produced, keeping the head and
the tail and reporting how much fell between.

**The privacy claim is precise.** `AGENT.md`, `USER.md` and `instructions/`
enter every turn's system prompt whoever is listening, and they have to: an
audience-dependent prompt would stop the prefix being cacheable and make every
turn of a long session cost more than the last. So the memory split covers what
is recalled and what is written down, and discretion about the workspace files
is asked of the model in the per-turn shared-room notice. README.md now says
this in those words instead of implying the scope covers everything.

**Real-model evidence sits beside the deterministic suite.** The scripted evals
verify request construction and orchestration; they cannot show that a model
obeys what it is sent. `internal/evals/live_test.go` runs four cases against
this machine's configured chain behind `FACTOR_LIVE_EVALS=1`, each planting
something specific and asking a question whose wrong answer is unmistakable:
the passport number in `USER.md` withheld in company and given to the user
alone, an answer that had to come from a file, a refusal outside the workspace
taken as an answer, and a spoken reply with no markdown in it.

**Platform integration has an execution requirement.** Installing Xvfb, scrot,
xdotool and xlogo was never the same claim as running the tests that need them.
CI now sets `FACTOR_REQUIRE_LIVE_DESKTOP=1`, which turns every environment skip
in `grid_live_test.go` into a failure. A machine without the helpers skips as
before.

## Validation

The short suite passes without an environment override:

```bash
go test -short ./...
```

`TestNewStatusUsesTheTerminalWidth` used to fail under an inherited `NO_COLOR`,
because it set `TERM` and left `NO_COLOR` alone while the behaviour it asserts
depends on both. It now sets both.

Each probe the audit ran twice is a test in the suite:

| Probe | Test |
|---|---|
| Other-process spending | `cost.TestSnapshotSeesWhatAnotherProcessSpent`, `TestConcurrentLedgersLoseNothing` |
| Shared system-channel scope | `memory.TestMachineTurnsObeyASharedAudience` |
| Compaction metadata failure | `session.TestCompactThatCannotCommitLeavesTheHistoryWhole`, `TestInterruptedCompactionIsFinishedOnRead` |
| Fresh job engine | `jobs.TestJobIDsAreUniqueAcrossEngines` |
| Background deny pattern | `jobs.TestBackgroundExecObeysTheDenyPatterns` |
| Failed connector startup | `channel.TestAFailedConnectorIsNotAnAddress` |

The rest of the work carries its own tests, one per claim:

| Claim | Test |
|---|---|
| A shared machine turn recalls and stores only in the room | `memory.TestASharedMachineTurnRecallsAndStoresInTheRoom` |
| …and recalls nothing where it cannot isolate | `memory.TestASharedMachineTurnWithNothingToIsolateIntoRecallsNothing` |
| A job records the room it was asked in | `jobs.TestJobStartRecordsTheRoomItWasAskedIn` |
| A task sub-turn runs under it | `jobs.TestTaskJobsRunUnderTheOriginAudience` |
| A delayed result is rescoped to the room it lands in | `agent.TestADelayedResultIsScopedToTheRoomItLandsIn` |
| …and the channel can widen but not narrow | `agent.TestTheChannelCanWidenAnAudienceButNotNarrowIt` |
| Cron and the heartbeat are scoped to their outlet | `agent.TestWorkNobodyAskedForIsScopedToTheRoomItReportsInto` |
| The manager asks a connector who can hear | `channel.TestManagerAsksAConnectorWhoCanHearTheReply` |
| Declaring company re-runs the turn in the room | `voice.TestDeclaringCompanyMidTurnRunsTheTurnAgainUnderTheSharedRoom` |
| …once, and a barge beats it | `voice.TestDeclaringCompanyInASharedTurnDoesNotReRunIt`, `TestABargeBeatsAPendingRescope` |
| The room is what the channel reports as the audience | `voice.TestVoiceReportsTheRoomAsTheAudience` |
| Calls in flight count against the cap | `cost.TestCallsInFlightCountAgainstTheCap` |
| A snapshot adds pending charges to what is on disk | `cost.TestSnapshotAddsPendingChargesToWhatIsOnDisk` |
| The scratch file is not shared between processes | `cost.TestTheScratchFileIsNotShared` |
| An append finishes an interrupted compaction first | `session.TestAppendFinishesAnInterruptedCompactionFirst` |
| Compaction keeps the time the session was last spoken into | `session.TestCompactionKeepsTheTimeTheSessionWasLastSpokenInto` |
| A denied command reaches the model as a refusal | `jobs.TestJobStartSurfacesADeniedCommand` |
| The reload waits for a delivery that left the queue | `gateway.TestSettleWaitsForADeliveryThatHasLeftTheQueue` |
| A refused restart notice is kept | `gateway.TestARefusedRestartNoticeIsKeptForTheNextStart` |
| A failed connector is named in the status rows and `/health` | `gateway.TestStatusLinesNameWhatRunsAndWhatDoesNot`, `TestRunStartsConfiguredChannelsAndHeartbeat` |
| Exec holds only its budget while the command runs | `tools.TestBoundedOutputHoldsOnlyItsBudget` |

Two of them are evals rather than unit tests, because the invariant is about
what reaches the model: `evals.TestTheSharedRoomNoticeRidesTheTurnAndNotThePrompt`
holds the system prompt byte-identical across audiences while the discretion
notice rides the turn context, and
`evals.TestTheSharedRoomNoticeCarriesItsOwnCorrection` keeps the way to correct
the notice in the same paragraph that makes the claim.

`make check` and `make lint` pass; coverage is 92%. Live desktop and browser
integrations, external services and real-model behaviour were not run here — the
live evals above are how the last of those is checked.
