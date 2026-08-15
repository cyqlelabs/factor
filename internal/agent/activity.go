package agent

// Phase is what a turn is busy with at a given moment. Front-ends map these
// onto whatever they draw while the user waits.
type Phase string

const (
	PhaseContext    Phase = "context"    // assembling the prompt: skills, memory recall
	PhaseThinking   Phase = "thinking"   // waiting on the provider
	PhaseTool       Phase = "tool"       // running a tool call (Detail is its name)
	PhaseNotice     Phase = "notice"     // the agent said what it is about to do (Detail is that line)
	PhaseCompacting Phase = "compacting" // summarizing history after a context overflow
	PhaseSteering   Phase = "steering"   // folding in a message that arrived mid-turn
	PhaseDone       Phase = "done"       // the turn ended, with a reply or an error
)

// Activity is one phase change on one session. Detail carries whatever the
// phase names: the tool being run, or the line the agent wants the user to
// read while it works.
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
