package memory

import (
	"context"
	"log/slog"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

// Bridge spaces: what the private and shared halves of the graph have in
// common, grown into a subspace of its own.
//
// Scoping recall by audience partitions the graph, and a partition has a cost
// the read overlay alone does not pay. The same person, project or plan
// discussed once alone and once with company becomes two unconnected islands:
// each side holds atoms about it, neither side's atoms link to the other's,
// and graph expansion — where smrti's recall gets most of its quality — stops
// at the boundary. smrti answers this natively. A bridge materializes the
// conceptual intersection of two spaces as new atoms with merged truth
// values, and edges from each bridge atom back to *both* parents, so
// traversal crosses the boundary that retrieval must not.
//
// It is deliberately not a way around the partition. The bridge holds what
// the two spaces already agreed on, which is by construction not private to
// either; nothing that exists only in the private space is copied out.
//
// The engine already does this on its own — run_epoch materializes bridges
// every tenth epoch — so this is not the only thing keeping the two halves
// joined, and it deliberately does not run on a schedule of its own. Two
// timers for one job in two repositories drift apart, and the engine's is the
// one that keeps working when Factor is not running. What Factor adds is
// timing the engine cannot know: the merge is worth the most the moment a
// gathering ends, which is when the two spaces have just finished diverging
// and when nobody is waiting on a reply. Everything else is left to the
// engine's own cycle.

const (
	// bridgeQuiet is how long the engine must have been untouched before a
	// merge starts. A bridge is never worth making a turn wait: it scans the
	// most salient 500 atoms of each space and compares them pairwise, with
	// an embedding of each atom's neighbourhood along the way.
	bridgeQuiet = 30 * time.Second

	// bridgeWait is how long to keep waiting for that quiet. The signal
	// arrives the instant a gathering ends, which is exactly when the turn
	// that ended it is still being extracted, so the wait has to outlast an
	// extraction — otherwise every merge would be skipped for being early
	// and lost until the next gathering.
	bridgeWait = 10 * time.Minute

	// bridgePoll is how often the wait re-checks for quiet.
	bridgePoll = 15 * time.Second

	// bridgeMinJaccard is how much overlap is worth materializing. Below it
	// the two spaces have nothing real in common and the "shared" concepts
	// are embedding noise, which would seed the graph with atoms nobody said.
	bridgeMinJaccard = 0.1
)

// SpaceMerger is the optional capability of an engine that can grow a bridge
// between two spaces. It is optional rather than part of Engine because an
// older smrti has no such route: without it the partition simply stands
// unbridged until the engine's own epoch gets to it, which is a loss of
// recall quality and never of correctness.
type SpaceMerger interface {
	MergeSpaces(ctx context.Context, space, other string, minJaccard float64) (int, error)
}

// idler is the half of the engine that reports quiet, for the merge to wait on.
type idler interface {
	Idle(quiet time.Duration) bool
}

// gatheringEnded records this turn's audience and reports whether it is the
// turn that ended a gathering — the shared-to-private edge, per channel.
// Anything else is not a moment worth bridging on: a gathering starting has
// produced nothing to join yet, and one continuing has not finished.
func (a *Ambient) gatheringEnded(channel, audience string) bool {
	a.audienceMu.Lock()
	defer a.audienceMu.Unlock()
	if a.lastAudience == nil {
		a.lastAudience = map[string]string{}
	}
	was := a.lastAudience[channel]
	a.lastAudience[channel] = audience
	return was == tools.AudienceShared && audience != tools.AudienceShared
}

// signalBridge asks the watcher to merge, without waiting for it and without
// queueing a second request behind one already pending: two gatherings ending
// before either merge runs still only need one merge.
func (a *Ambient) signalBridge() {
	select {
	case a.bridgeNow <- struct{}{}:
	default:
	}
}

// WatchBridges merges the private and shared spaces whenever a gathering
// ends, for as long as ctx lives. It returns immediately when there is
// nothing to bridge — no shared space configured, or an engine that cannot
// merge — so a caller can start it unconditionally.
func (a *Ambient) WatchBridges(ctx context.Context) {
	if !a.canBridge() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.bridgeNow:
			a.bridgeWhenQuiet(ctx)
		}
	}
}

func (a *Ambient) canBridge() bool {
	if a == nil || a.Engine == nil || a.Spaces.Main == "" || a.Spaces.Shared == "" {
		return false
	}
	_, ok := a.Engine.(SpaceMerger)
	return ok
}

// bridgeWhenQuiet waits out the turn that just ended before merging, then
// gives up rather than holding the merge over a machine that never settles.
// Losing one bridge costs recall quality until the engine's own epoch runs;
// competing with a live conversation costs the conversation.
func (a *Ambient) bridgeWhenQuiet(ctx context.Context) {
	quiet, checkable := a.Engine.(idler)
	deadline := time.Now().Add(bridgeWait)
	for checkable && !quiet.Idle(bridgeQuiet) {
		if time.Now().After(deadline) {
			slog.Debug("memory bridge skipped: the engine stayed busy")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(bridgePoll):
		}
	}
	a.bridgeOnce(ctx)
}

// bridgeOnce grows the bridge if the engine is up and routing spaces. Every
// reason to skip is a normal state rather than a failure: the engine
// restarts, and spaces are a newer feature than some installs.
func (a *Ambient) bridgeOnce(ctx context.Context) {
	merger, ok := a.Engine.(SpaceMerger)
	if !ok || !a.Engine.Healthy() {
		return
	}
	if routes, space := a.Engine.SpaceSupport(); !routes || space != a.Spaces.Main {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	created, err := merger.MergeSpaces(ctx, a.Spaces.Main, a.Spaces.Shared, bridgeMinJaccard)
	if err != nil {
		slog.Debug("memory bridge skipped", "error", err)
		return
	}
	if created > 0 {
		slog.Info("memory bridge grown", "spaces", a.Spaces.Main+" × "+a.Spaces.Shared,
			"atoms", created)
	}
}
