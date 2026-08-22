package memory

import (
	"context"
	"log/slog"
	"time"
)

// Bridge spaces: what the private and shared halves of the graph have in
// common, grown into a subspace of its own.
//
// Scoping recall by audience partitions the graph, and a partition has a
// cost the read overlay alone does not pay. The same person, project or plan
// discussed once alone and once with company becomes two unconnected islands:
// each side holds atoms about it, neither side's atoms link to the other's,
// and graph expansion — which is where smrti's recall gets most of its
// quality — stops at the boundary. smrti answers this natively. A bridge
// materializes the conceptual intersection of two spaces as new atoms with
// merged truth values, and edges from each bridge atom back to *both*
// parents, so traversal crosses the boundary that retrieval must not.
//
// It is deliberately not a way around the partition. The bridge holds what
// the two spaces already agreed on, which is by construction not private to
// either; nothing that exists only in the private space is copied out.

const (
	// defaultBridgeInterval is how often the bridge is regrown. Slow on
	// purpose: materializing one scans the salient atoms of both spaces and
	// embeds their neighborhoods, which is real work for an engine whose
	// day job is answering turns, and the answer barely moves hour to hour.
	defaultBridgeInterval = 6 * time.Hour

	// bridgeQuiet is how long the engine must have been untouched before a
	// merge starts. A bridge is never worth making a turn wait.
	bridgeQuiet = 30 * time.Second

	// bridgeMinJaccard is how much overlap is worth materializing. Below it
	// the two spaces have nothing real in common and the "shared" concepts
	// are embedding noise, which would seed the graph with atoms nobody
	// said.
	bridgeMinJaccard = 0.1
)

// SpaceMerger is the optional capability of an engine that can grow a bridge
// between two spaces. It is optional rather than part of Engine because an
// older smrti has no such route: the feature degrades to the partition
// without it, which is a loss of recall quality and never of correctness.
type SpaceMerger interface {
	MergeSpaces(ctx context.Context, space, other string, minJaccard float64) (int, error)
}

// idler is the half of the engine that reports quiet, for the merge to wait on.
type idler interface {
	Idle(quiet time.Duration) bool
}

// WatchBridges regrows the private/shared bridge for as long as ctx lives.
// It returns immediately when there is nothing to bridge — no shared space
// configured, or an engine that cannot merge — so a caller can start it
// unconditionally.
func (a *Ambient) WatchBridges(ctx context.Context, every time.Duration) {
	if !a.canBridge() {
		return
	}
	if every <= 0 {
		every = defaultBridgeInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.bridgeOnce(ctx)
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

// bridgeOnce grows the bridge if the engine is up, routing spaces, and quiet.
// Every reason to skip is a normal state rather than a failure: the engine
// restarts, spaces are a newer feature than some installs, and a machine in
// the middle of a conversation is exactly when not to do this.
func (a *Ambient) bridgeOnce(ctx context.Context) {
	merger, ok := a.Engine.(SpaceMerger)
	if !ok || !a.Engine.Healthy() {
		return
	}
	if routes, space := a.Engine.SpaceSupport(); !routes || space != a.Spaces.Main {
		return
	}
	if quiet, ok := a.Engine.(idler); ok && !quiet.Idle(bridgeQuiet) {
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
