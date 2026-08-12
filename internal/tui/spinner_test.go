package tui

import (
	"strings"
	"testing"
	"time"
)

// statusConsole is a console that only collects the activity line.
func statusConsole() (*Console, *syncBuf) {
	out := &syncBuf{}
	c := &Console{out: out, width: func() int { return 200 }}
	c.paint = true
	return c, out
}

// frozen makes the spinner's clock stand still until a test moves it.
func frozen(s *Spinner) *time.Duration {
	var elapsed time.Duration
	base := time.Unix(0, 0)
	s.now = func() time.Time { return base.Add(elapsed) }
	return &elapsed
}

func TestSpinnerLineNamesEveryPhase(t *testing.T) {
	con, _ := statusConsole()
	s := NewSpinner(con)
	elapsed := frozen(s)
	s.Start()
	defer s.Stop()

	cases := []struct {
		phase, detail, want string
	}{
		{phaseContext, "", "gathering context"},
		{phaseThinking, "", "thinking"},
		{phaseTool, "read_file", "running read_file"},
		{phaseTool, "", "running a tool"},
		{phaseCompacting, "", "compacting the conversation"},
		{phaseSteering, "", "folding in what you typed"},
		{"unheard-of", "", "thinking"},
	}
	for _, tc := range cases {
		s.Set(tc.phase, tc.detail)
		*elapsed = 4200 * time.Millisecond
		line := s.line()
		if !strings.Contains(line, tc.want) {
			t.Errorf("phase %q line = %q, want %q in it", tc.phase, line, tc.want)
		}
		if !strings.Contains(line, "4.2s") {
			t.Errorf("phase %q line = %q, want the elapsed time", tc.phase, line)
		}
	}
}

func TestSpinnerLineCountsStepsAndOffersSteering(t *testing.T) {
	con, _ := statusConsole()
	s := NewSpinner(con)
	elapsed := frozen(s)
	s.Start()
	defer s.Stop()

	if strings.Contains(s.line(), "step") {
		t.Errorf("a fresh turn should not count steps: %q", s.line())
	}
	s.Set(phaseTool, "read_file")
	if !strings.Contains(s.line(), "1 step") || strings.Contains(s.line(), "1 steps") {
		t.Errorf("line = %q, want a singular step", s.line())
	}
	s.Set(phaseTool, "exec_command")
	if !strings.Contains(s.line(), "2 steps") {
		t.Errorf("line = %q, want two steps", s.line())
	}

	// The steering hint only shows up once the wait is long enough to need it.
	if strings.Contains(s.line(), "type to steer") {
		t.Errorf("hint shown too early: %q", s.line())
	}
	*elapsed = hintAfter
	if !strings.Contains(s.line(), "type to steer") {
		t.Errorf("hint missing after %s: %q", hintAfter, s.line())
	}
}

func TestSpinnerSummarizesTheTurn(t *testing.T) {
	con, _ := statusConsole()
	s := NewSpinner(con)
	elapsed := frozen(s)

	s.Start()
	s.Set(phaseTool, "read_file")
	s.Set(phaseTool, "exec_command")
	s.Set(phaseTool, "read_file") // the same tool twice is one name, two steps
	s.Set(phaseThinking, "")
	*elapsed = 3 * time.Second

	sum := s.Stop()
	if sum.Steps != 3 {
		t.Errorf("steps = %d, want 3", sum.Steps)
	}
	if len(sum.Tools) != 2 || sum.Tools[0] != "read_file" || sum.Tools[1] != "exec_command" {
		t.Errorf("tools = %v", sum.Tools)
	}
	if sum.Note() != "3.0s · read_file, exec_command" {
		t.Errorf("note = %q", sum.Note())
	}
	if s.Running() {
		t.Error("still running after Stop")
	}
}

func TestSpinnerIgnoresUpdatesOutsideATurn(t *testing.T) {
	con, out := statusConsole()
	s := NewSpinner(con)

	s.Set(phaseTool, "read_file") // before Start
	if out.String() != "" {
		t.Errorf("painted while idle: %q", out.String())
	}
	if sum := s.Stop(); sum.Steps != 0 || len(sum.Tools) != 0 || sum.Note() != "" {
		t.Errorf("stopping an idle spinner = %+v", sum)
	}

	s.Start()
	started := out.String()
	s.Start() // a steering message must not restart the clock
	if out.String() != started {
		t.Errorf("second Start repainted: %q", out.String())
	}
	s.Stop()
	if sum := s.Stop(); sum.Elapsed != 0 || sum.Steps != 0 || len(sum.Tools) != 0 {
		t.Errorf("second Stop = %+v, want nothing", sum)
	}
}

func TestSpinnerAnimatesUntilStopped(t *testing.T) {
	con, out := statusConsole()
	s := NewSpinner(con)
	s.tick = time.Millisecond
	s.Start()

	// The wave has to actually move: wait for a frame other than the first.
	first := waveFrame(0)
	deadline := time.Now().Add(5 * time.Second)
	moved := false
	for !moved && time.Now().Before(deadline) {
		painted := out.String()
		moved = strings.Contains(painted, waveFrame(1)) || strings.Contains(painted, waveFrame(2))
		time.Sleep(time.Millisecond)
	}
	if !moved {
		t.Fatalf("the pulse never advanced past %q: %q", first, out.String())
	}

	out.Reset()
	s.Stop()
	frozen := out.String()
	time.Sleep(20 * time.Millisecond)
	if out.String() != frozen {
		t.Errorf("still painting after Stop: %q", out.String())
	}
	// Whatever frame was mid-flight, Stop's last word is erasing the line.
	if !strings.HasSuffix(frozen, "\x1b[J") {
		t.Errorf("activity line not cleared: %q", frozen)
	}
}

func TestWaveFrameTravels(t *testing.T) {
	first := waveFrame(0)
	if len([]rune(first)) != waveWidth {
		t.Fatalf("frame %q is %d cells, want %d", first, len([]rune(first)), waveWidth)
	}
	if waveFrame(1) == first {
		t.Error("consecutive frames are identical")
	}
	if waveFrame(len(waveCells)) != first {
		t.Error("the wave does not loop cleanly")
	}
}

func TestSummaryNote(t *testing.T) {
	cases := []struct {
		name string
		sum  Summary
		want string
	}{
		{"a quick plain turn says nothing", Summary{Elapsed: 900 * time.Millisecond}, ""},
		{"a slow turn reports its time", Summary{Elapsed: 5*time.Second + 400*time.Millisecond}, "5.4s"},
		{"tools are always worth naming", Summary{Elapsed: time.Second, Tools: []string{"web_search"}}, "1.0s · web_search"},
		{"minutes read as minutes", Summary{Elapsed: 95 * time.Second}, "1m35s"},
		{
			"long tool lists are cut short",
			Summary{Elapsed: time.Minute, Tools: []string{"a", "b", "c", "d", "e", "f"}},
			"1m00s · a, b, c, d, +2 more",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sum.Note(); got != tc.want {
				t.Errorf("note = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatElapsedNeverGoesNegative(t *testing.T) {
	if got := formatElapsed(-time.Second); got != "0.0s" {
		t.Errorf("formatElapsed(-1s) = %q", got)
	}
}

func TestPhaseColorsDifferByPhase(t *testing.T) {
	seen := map[string]string{}
	for _, phase := range []string{phaseThinking, phaseTool, phaseCompacting, phaseSteering} {
		seen[phase] = phaseColor(phase)
	}
	if seen[phaseTool] == seen[phaseThinking] || seen[phaseCompacting] == seen[phaseSteering] {
		t.Errorf("phases share a color: %v", seen)
	}
}
