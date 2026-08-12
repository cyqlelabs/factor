package agent

// Phase is what a turn is busy with at a given moment. Front-ends map these
// onto whatever they draw while the user waits.
type Phase string

const (
	PhaseContext    Phase = "context"    // assembling the prompt: skills, memory recall
	PhaseThinking   Phase = "thinking"   // waiting on the provider
	PhaseTool       Phase = "tool"       // running a tool call (Detail is its name)
	PhaseCompacting Phase = "compacting" // summarizing history after a context overflow
	PhaseSteering   Phase = "steering"   // folding in a message that arrived mid-turn
	PhaseDone       Phase = "done"       // the turn ended, with a reply or an error
)

// Activity is one phase change on one session.
type Activity struct {
	SessionKey string
	Phase      Phase
	Detail     string
}

// OnActivity installs the activity watcher, replacing any previous one and
// clearing it when fn is nil. It is called from turn goroutines, so it must
// not block and must not call back into the loop.
func (l *Loop) OnActivity(fn func(Activity)) {
	l.watchMu.Lock()
	defer l.watchMu.Unlock()
	l.watcher = fn
}

func (l *Loop) emit(sessionKey string, phase Phase, detail string) {
	l.watchMu.RLock()
	fn := l.watcher
	l.watchMu.RUnlock()
	if fn != nil {
		fn(Activity{SessionKey: sessionKey, Phase: phase, Detail: detail})
	}
}
