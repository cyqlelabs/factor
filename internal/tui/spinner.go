package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Phase names mirror agent.Phase. They are plain strings so the tui package
// stays independent of the agent package.
const (
	phaseContext    = "context"
	phaseThinking   = "thinking"
	phaseTool       = "tool"
	phaseCompacting = "compacting"
	phaseSteering   = "steering"
)

// waveCells are the levels the pulse rolls through: a wave travelling
// left along the line for as long as the agent is working.
var waveCells = []rune("▁▂▄▆█▆▄▂")

const (
	waveWidth     = 7
	frameInterval = 90 * time.Millisecond
	hintAfter     = 6 * time.Second
	maxToolNames  = 4
)

// Summary describes a finished turn.
type Summary struct {
	Elapsed time.Duration
	Steps   int
	Tools   []string
}

// Note renders the summary as the dim one-liner under a reply, or "" when a
// turn was quick and plain enough that saying anything would be noise.
func (s Summary) Note() string {
	if len(s.Tools) == 0 && s.Elapsed < 2*time.Second {
		return ""
	}
	note := formatElapsed(s.Elapsed)
	if len(s.Tools) > 0 {
		shown := s.Tools
		if len(shown) > maxToolNames {
			shown = append(append([]string(nil), shown[:maxToolNames]...),
				fmt.Sprintf("+%d more", len(s.Tools)-maxToolNames))
		}
		note += " · " + strings.Join(shown, ", ")
	}
	return note
}

// Spinner drives the activity line: a pulse that travels while the agent
// works, what it is doing, and how long it has been at it.
type Spinner struct {
	con  *Console
	tick time.Duration
	now  func() time.Time

	mu      sync.Mutex
	running bool
	start   time.Time
	phase   string
	detail  string
	steps   int
	tools   []string
	frame   int
	stop    chan struct{}
	done    chan struct{}
}

// NewSpinner returns a spinner that paints on con.
func NewSpinner(con *Console) *Spinner {
	return &Spinner{con: con, tick: frameInterval, now: time.Now}
}

// Start begins a turn. Calling it on a running spinner does nothing, so a
// steering message folded into a live turn keeps the same elapsed clock.
func (s *Spinner) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running, s.start = true, s.now()
	s.phase, s.detail, s.steps, s.tools, s.frame = phaseThinking, "", 0, nil, 0
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	stop, done := s.stop, s.done
	s.mu.Unlock()

	s.paint()
	go func() {
		ticker := time.NewTicker(s.tick)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.mu.Lock()
				s.frame++
				s.mu.Unlock()
				s.paint()
			}
		}
	}()
}

// Set moves the spinner to a new phase; detail carries the tool name.
func (s *Spinner) Set(phase, detail string) {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	if phase == phaseTool && detail != "" {
		s.steps++
		if !contains(s.tools, detail) {
			s.tools = append(s.tools, detail)
		}
	}
	s.phase, s.detail = phase, detail
	s.mu.Unlock()
	s.paint()
}

// Stop ends the turn, clears the activity line, and reports what it took.
func (s *Spinner) Stop() Summary {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return Summary{}
	}
	s.running = false
	close(s.stop)
	done := s.done
	sum := Summary{Elapsed: s.now().Sub(s.start), Steps: s.steps, Tools: append([]string(nil), s.tools...)}
	s.mu.Unlock()

	<-done
	s.con.SetStatus("")
	return sum
}

// Running reports whether a turn is in flight.
func (s *Spinner) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Spinner) paint() { s.con.SetStatus(s.line()) }

func (s *Spinner) line() string {
	s.mu.Lock()
	frame, phase, detail, steps := s.frame, s.phase, s.detail, s.steps
	elapsed := s.now().Sub(s.start)
	s.mu.Unlock()

	line := "  " + s.con.style(phaseColor(phase), waveFrame(frame)) + "  " + s.label(phase, detail)
	line += s.con.style(ansiDim, " · "+formatElapsed(elapsed))
	if steps > 0 {
		line += s.con.style(ansiDim, fmt.Sprintf(" · %d %s", steps, plural(steps, "step")))
	}
	if elapsed >= hintAfter {
		line += s.con.style(ansiDim, "   type to steer")
	}
	return line
}

func (s *Spinner) label(phase, detail string) string {
	switch phase {
	case phaseContext:
		return "gathering context"
	case phaseTool:
		if detail == "" {
			return "running a tool"
		}
		return "running " + s.con.style(ansiBold, detail)
	case phaseCompacting:
		return "compacting the conversation"
	case phaseSteering:
		return "folding in what you typed"
	default:
		return "thinking"
	}
}

func phaseColor(phase string) string {
	switch phase {
	case phaseTool:
		return ansiYellow
	case phaseCompacting:
		return ansiMagenta
	case phaseSteering:
		return ansiGreen
	default:
		return ansiCyan
	}
}

// waveFrame renders the pulse at frame t.
func waveFrame(t int) string {
	cells := make([]rune, waveWidth)
	for i := range cells {
		cells[i] = waveCells[(t+i)%len(waveCells)]
	}
	return string(cells)
}

func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
