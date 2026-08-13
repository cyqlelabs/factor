package tui

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is an io.Writer a test can read while the console writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuf) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// rawConsole builds a console with the raw editor forced on over a pipe, so
// key handling and repainting can be driven without a real terminal.
func rawConsole(t *testing.T) (*Console, *os.File, *syncBuf) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	out := &syncBuf{}
	c := &Console{in: r, out: out, width: func() int { return 40 }}
	c.paint, c.rawInput = true, true
	c.prompt = "you> "
	return c, w, out
}

// line drives one ReadLine, feeding it keys, and returns what it produced.
func (c *Console) readWith(t *testing.T, w *os.File, keys ...string) (string, error) {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := c.ReadLine()
		done <- result{line, err}
	}()
	for _, k := range keys {
		if _, err := w.WriteString(k); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case r := <-done:
		return r.line, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("ReadLine never returned")
		return "", nil
	}
}

func TestReadLineEditsTheBuffer(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{"plain typing", []string{"hello\r"}, "hello"},
		{"backspace", []string{"helllo", "\x7f\x7f", "o\r"}, "hello"},
		{"insert at the cursor", []string{"helo", "\x1b[D", "l\r"}, "hello"},
		{"home then typing", []string{"ello", "\x01", "h\r"}, "hello"},
		{"end after home", []string{"hell", "\x01", "\x05", "o\r"}, "hello"},
		{"kill to the end", []string{"hello there", "\x01", "\x05", "\x0b", "\r"}, "hello there"},
		{"clear the line", []string{"garbage", "\x15", "hello\r"}, "hello"},
		{"kill the last word", []string{"hello wide world", "\x17", "\r"}, "hello wide "},
		{"delete forward", []string{"xhello", "\x01", "\x1b[3~", "\r"}, "hello"},
		{"ctrl-d deletes forward mid-line", []string{"xhello", "\x01", "\x04", "\r"}, "hello"},
		{"multi-byte runes", []string{"héllo ✦\r"}, "héllo ✦"},
		{"unknown escapes are dropped", []string{"he", "\x1b[1;5C", "llo\r"}, "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w, _ := rawConsole(t)
			line, err := c.readWith(t, w, tc.keys...)
			if err != nil {
				t.Fatalf("ReadLine = %v", err)
			}
			if line != tc.want {
				t.Errorf("line = %q, want %q", line, tc.want)
			}
		})
	}
}

func TestNewlineShortcutsBuildAMultiLineMessage(t *testing.T) {
	cases := []struct {
		name string
		keys []string
	}{
		{"ctrl-j", []string{"first\nsecond\r"}},
		{"alt-enter", []string{"first", "\x1b\r", "second\r"}},
		{"alt-enter as escape-newline", []string{"first", "\x1b\n", "second\r"}},
		{"kitty shift-enter", []string{"first", "\x1b[13;2u", "second\r"}},
		{"xterm shift-enter", []string{"first", "\x1b[27;2;13~", "second\r"}},
		{"a trailing backslash continues", []string{"first\\", "\r", "second\r"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w, _ := rawConsole(t)
			line, err := c.readWith(t, w, tc.keys...)
			if err != nil {
				t.Fatalf("ReadLine = %v", err)
			}
			if line != "first\nsecond" {
				t.Errorf("line = %q, want a two-line message", line)
			}
		})
	}
}

func TestMultiLineInputIsDrawnWithAContinuationGuide(t *testing.T) {
	c, w, out := rawConsole(t)
	go func() { _, _ = c.ReadLine() }()
	if _, err := w.WriteString("first\nsecond"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "second") })

	painted := stripANSI(out.String())
	if !strings.Contains(painted, "you> first\r\n   ┆ second") {
		t.Errorf("continuation row not drawn: %q", painted)
	}
	// Printing over a two-row input has to climb back over both rows.
	out.Reset()
	c.Printf("a message")
	if !strings.Contains(out.String(), "\x1b[1A") {
		t.Errorf("the second input row was not walked back: %q", out.String())
	}
	if !strings.Contains(stripANSI(out.String()), "   ┆ second") {
		t.Errorf("input not repainted after printing: %q", stripANSI(out.String()))
	}
	_, _ = w.WriteString("\r")
}

func TestBackslashOnlyContinuesAtTheEndOfTheLine(t *testing.T) {
	c, w, _ := rawConsole(t)
	// With the cursor parked mid-line, a trailing backslash still sends.
	line, err := c.readWith(t, w, "a\\b", "\x1b[D", "\r")
	if err != nil {
		t.Fatalf("ReadLine = %v", err)
	}
	if line != "a\\b" {
		t.Errorf("line = %q, want the message sent as typed", line)
	}
}

func TestPastedLinesAreNotLost(t *testing.T) {
	c, w, _ := rawConsole(t)
	// Three lines arrive in one paste: each ReadLine takes the next one.
	if _, err := w.WriteString("first\rsecond\rthird\r"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first", "second", "third"} {
		line, err := c.ReadLine()
		if err != nil || line != want {
			t.Fatalf("ReadLine = %q, %v; want %q", line, err, want)
		}
	}
}

func TestReadLineAcceptsRunesSplitAcrossReads(t *testing.T) {
	c, w, _ := rawConsole(t)
	// "é" arrives one byte at a time: the decoder must wait for the second.
	line, err := c.readWith(t, w, "caf", "\xc3", "\xa9", "\r")
	if err != nil {
		t.Fatalf("ReadLine = %v", err)
	}
	if line != "café" {
		t.Errorf("line = %q", line)
	}
}

func TestReadLineWalksHistory(t *testing.T) {
	// Each case starts from the same two-entry history; submitting a recalled
	// line grows it, so every walk gets a fresh console.
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{"one up is the newest entry", []string{"\x1b[A", "\r"}, "two"},
		{"two ups reach the oldest", []string{"\x1b[A", "\x1b[A", "\r"}, "one"},
		{"down comes back", []string{"\x1b[A", "\x1b[A", "\x1b[B", "\r"}, "two"},
		{"past the oldest stays put", []string{"\x1b[A", "\x1b[A", "\x1b[A", "\r"}, "one"},
		{"down restores the stashed draft", []string{"draft", "\x1b[A", "\x1b[B", "\r"}, "draft"},
		{"a recalled line can be edited", []string{"\x1b[A", "!", "\r"}, "two!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w, _ := rawConsole(t)
			for _, seed := range []string{"one", "two"} {
				if line, err := c.readWith(t, w, seed+"\r"); err != nil || line != seed {
					t.Fatalf("seeding history: %q, %v", line, err)
				}
			}
			if line, _ := c.readWith(t, w, tc.keys...); line != tc.want {
				t.Errorf("line = %q, want %q", line, tc.want)
			}
		})
	}
}

func TestHistorySkipsBlanksAndRepeats(t *testing.T) {
	c, w, _ := rawConsole(t)
	for _, seed := range []string{"same", "same", "   "} {
		if _, err := c.readWith(t, w, seed+"\r"); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	hist := append([]string(nil), c.hist...)
	c.mu.Unlock()
	if len(hist) != 1 || hist[0] != "same" {
		t.Errorf("history = %q, want one entry", hist)
	}
}

func TestReadLineInterruptAndEOF(t *testing.T) {
	c, w, out := rawConsole(t)

	// Ctrl-C with text pending only clears the line.
	if line, err := c.readWith(t, w, "junk", "\x03", "kept\r"); err != nil || line != "kept" {
		t.Fatalf("line = %q, err = %v", line, err)
	}
	out.Reset()
	if _, err := c.readWith(t, w, "\x03"); !errors.Is(err, ErrInterrupted) {
		t.Errorf("ctrl-c on an empty line = %v, want ErrInterrupted", err)
	}
	if !strings.Contains(out.String(), "^C") {
		t.Errorf("interrupt not shown: %q", out.String())
	}
	if _, err := c.readWith(t, w, "\x04"); !errors.Is(err, io.EOF) {
		t.Errorf("ctrl-d on an empty line = %v, want EOF", err)
	}
}

func TestReadLineReturnsEOFWhenInputCloses(t *testing.T) {
	c, w, _ := rawConsole(t)
	go func() {
		_, _ = w.WriteString("half typed")
		time.Sleep(20 * time.Millisecond)
		_ = w.Close()
	}()
	if _, err := c.ReadLine(); !errors.Is(err, io.EOF) {
		t.Errorf("ReadLine = %v, want EOF", err)
	}
}

func TestPrintedOutputKeepsHalfTypedInput(t *testing.T) {
	c, w, out := rawConsole(t)

	done := make(chan string, 1)
	go func() {
		line, _ := c.ReadLine()
		done <- line
	}()
	if _, err := w.WriteString("half"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "half") })

	out.Reset()
	c.Reply("here is the answer", "1.5s · read_file")
	painted := out.String()
	if !strings.Contains(painted, "here is the answer") {
		t.Fatalf("reply not printed: %q", painted)
	}
	if !strings.Contains(painted, "1.5s · read_file") {
		t.Errorf("meta note not printed: %q", painted)
	}
	// The half-typed line is repainted under the reply rather than lost.
	if !strings.Contains(stripANSI(painted), "you> half") {
		t.Errorf("prompt not restored after printing: %q", stripANSI(painted))
	}

	if _, err := w.WriteString("\r"); err != nil {
		t.Fatal(err)
	}
	if line := <-done; line != "half" {
		t.Errorf("line = %q, want the text typed before the reply", line)
	}
}

func TestStatusLineSitsAboveThePrompt(t *testing.T) {
	c, w, out := rawConsole(t)
	go func() { _, _ = c.ReadLine() }()
	waitFor(t, func() bool { return strings.Contains(out.String(), "you> ") })

	out.Reset()
	c.SetStatus("  ~~~ thinking")
	painted := stripANSI(out.String())
	if !strings.Contains(painted, "~~~ thinking\r\nyou> ") {
		t.Errorf("status not drawn above the prompt: %q", painted)
	}

	// An unchanged status does not repaint.
	out.Reset()
	c.SetStatus("  ~~~ thinking")
	if out.String() != "" {
		t.Errorf("repainted an unchanged status: %q", out.String())
	}

	// Clearing walks back over the status row before erasing.
	out.Reset()
	c.SetStatus("")
	if !strings.Contains(out.String(), "\x1b[1A") {
		t.Errorf("status row not walked back: %q", out.String())
	}
	if strings.Contains(stripANSI(out.String()), "thinking") {
		t.Errorf("status still drawn: %q", out.String())
	}

	_, _ = w.WriteString("\r")
}

func TestPromptSwitchesWhileATurnRuns(t *testing.T) {
	c, w, out := rawConsole(t)
	c.color = true
	go func() { _, _ = c.ReadLine() }()
	waitFor(t, func() bool { return strings.Contains(out.String(), "you> ") })

	out.Reset()
	c.PromptSteering()
	if !strings.Contains(stripANSI(out.String()), "steer› ") {
		t.Errorf("steering prompt not drawn: %q", out.String())
	}
	out.Reset()
	c.PromptIdle()
	if !strings.Contains(stripANSI(out.String()), "you› ") {
		t.Errorf("idle prompt not restored: %q", out.String())
	}
	_, _ = w.WriteString("\r")
}

func TestLongLineWrapsWithoutLosingTheCursorRow(t *testing.T) {
	c, w, out := rawConsole(t) // width 40
	go func() { _, _ = c.ReadLine() }()
	long := strings.Repeat("x", 70)
	if _, err := w.WriteString(long); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), long) })

	out.Reset()
	c.Printf("interrupting message")
	// prompt (5) + 70 chars = row 1 of the wrap: the erase must climb one row.
	if !strings.Contains(out.String(), "\x1b[1A") {
		t.Errorf("wrapped row not accounted for: %q", out.String())
	}
	_, _ = w.WriteString("\r")
}

func TestClearScreenRepaints(t *testing.T) {
	c, w, out := rawConsole(t)
	go func() { _, _ = c.ReadLine() }()
	waitFor(t, func() bool { return strings.Contains(out.String(), "you> ") })
	out.Reset()
	if _, err := w.WriteString("\x0c"); err != nil {
		t.Fatal(err)
	}
	// The clear sequence and the prompt repaint are separate writes; wait for both.
	waitFor(t, func() bool {
		return strings.Contains(out.String(), "\x1b[2J") && strings.Contains(stripANSI(out.String()), "you> ")
	})
	_, _ = w.WriteString("\r")
}

func TestPlainConsoleFallsBackToLines(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	out := &syncBuf{}

	c := NewChat(r, nil)
	c.out = out
	c.Start() // not a terminal: stays plain
	if c.Interactive() {
		t.Fatal("a pipe should not be interactive")
	}

	go func() {
		_, _ = w.WriteString("hello\n")
		_ = w.Close()
	}()
	line, err := c.ReadLine()
	if err != nil || line != "hello" {
		t.Fatalf("ReadLine = %q, %v", line, err)
	}

	c.SetStatus("thinking") // no live region to paint into
	c.Reply("an answer", "2.0s")
	c.Printf("a notice")
	got := out.String()
	if strings.Contains(got, "thinking") {
		t.Errorf("status painted without a terminal: %q", got)
	}
	for _, want := range []string{"you› ", "factor› an answer", "2.0s", "a notice"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("escape codes leaked into a pipe: %q", got)
	}

	if _, err := c.ReadLine(); !errors.Is(err, io.EOF) {
		t.Errorf("closed input = %v, want EOF", err)
	}
	c.Close()
}

func TestNewChatWithoutStreams(t *testing.T) {
	c := NewChat(nil, nil)
	c.Start()
	c.Printf("swallowed")
	c.SetStatus("swallowed")
	if _, err := c.ReadLine(); !errors.Is(err, io.EOF) {
		t.Errorf("ReadLine without input = %v, want EOF", err)
	}
	c.Close()
}

func TestNewStatusPaintsOnlyTheActivityLine(t *testing.T) {
	c := NewStatus(nil)
	out := &syncBuf{}
	c.out, c.width = out, func() int { return 40 }
	c.mu.Lock()
	c.paint = true
	c.mu.Unlock()

	c.SetStatus("working")
	if got := out.String(); got != "working" {
		t.Errorf("status = %q, want a bare line with no prompt", got)
	}
	out.Reset()
	c.SetStatus("")
	if !strings.Contains(out.String(), "\x1b[J") {
		t.Errorf("status not erased: %q", out.String())
	}
}

func TestStatusIsTruncatedToTheTerminalWidth(t *testing.T) {
	c, w, out := rawConsole(t)
	go func() { _, _ = c.ReadLine() }()
	waitFor(t, func() bool { return strings.Contains(out.String(), "you> ") })

	out.Reset()
	c.SetStatus(strings.Repeat("z", 100))
	painted := stripANSI(out.String())
	first, _, _ := strings.Cut(strings.TrimLeft(painted, "\r"), "\r\n")
	if len([]rune(first)) > 40 {
		t.Errorf("status line is %d cells wide, want at most the terminal width", len([]rune(first)))
	}
	if !strings.HasSuffix(first, "…") {
		t.Errorf("truncated status should be elided: %q", first)
	}
	_, _ = w.WriteString("\r")
}

func TestCloseErasesAndIsSafeWithoutRawMode(t *testing.T) {
	c, _, out := rawConsole(t)
	c.SetStatus("busy")
	out.Reset()
	c.Close()
	if !strings.Contains(out.String(), "\x1b[J") {
		t.Errorf("Close did not erase the live region: %q", out.String())
	}
}

func TestDecodeKey(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		size  int
		kind  keyKind
		rune_ rune
	}{
		{"empty", "", 0, keyIgnore, 0},
		{"enter", "\r", 1, keyEnter, 0},
		{"ctrl-j inserts a newline", "\n", 1, keyNewline, 0},
		{"alt-enter inserts a newline", "\x1b\r", 2, keyNewline, 0},
		{"kitty shift-enter", "\x1b[13;2u", 7, keyNewline, 0},
		{"xterm shift-enter", "\x1b[27;2;13~", 10, keyNewline, 0},
		{"backspace", "\x7f", 1, keyBackspace, 0},
		{"ctrl-h", "\x08", 1, keyBackspace, 0},
		{"ctrl-a", "\x01", 1, keyHome, 0},
		{"ctrl-b", "\x02", 1, keyLeft, 0},
		{"ctrl-e", "\x05", 1, keyEnd, 0},
		{"ctrl-f", "\x06", 1, keyRight, 0},
		{"rune", "a", 1, keyRune, 'a'},
		{"wide rune", "✦", 3, keyRune, '✦'},
		{"other control", "\x1a", 1, keyIgnore, 0},
		{"partial escape", "\x1b[", 0, keyIgnore, 0},
		{"partial rune", "\xc3", 0, keyIgnore, 0},
		{"invalid rune", "\xff\xff\xff\xff", 1, keyIgnore, 0},
		{"up", "\x1b[A", 3, keyUp, 0},
		{"down", "\x1b[B", 3, keyDown, 0},
		{"application-mode up", "\x1bOA", 3, keyUp, 0},
		{"home", "\x1b[H", 3, keyHome, 0},
		{"end", "\x1b[F", 3, keyEnd, 0},
		{"delete", "\x1b[3~", 4, keyDelete, 0},
		{"page down", "\x1b[6~", 4, keyIgnore, 0},
		{"modified arrow", "\x1b[1;5C", 6, keyIgnore, 0},
		{"alt-key", "\x1bxy", 2, keyIgnore, 0},
		{"unterminated csi", "\x1b[12", 0, keyIgnore, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, k := decodeKey([]byte(tc.in))
			if size != tc.size || k.kind != tc.kind || k.r != tc.rune_ {
				t.Errorf("decodeKey(%q) = %d, %v/%q; want %d, %v/%q",
					tc.in, size, k.kind, k.r, tc.size, tc.kind, tc.rune_)
			}
		})
	}
}

func TestStatusBarSitsUnderThePrompt(t *testing.T) {
	c, w, out := rawConsole(t)
	c.width = func() int { return 100 }
	go func() { _, _ = c.ReadLine() }()
	waitFor(t, func() bool { return strings.Contains(out.String(), "you> ") })

	out.Reset()
	c.SetBar(Bar{Session: "main", Model: "fake-model", Memory: "memory ✓",
		Hints: []string{"alt+⏎ newline"}})
	painted := stripANSI(out.String())
	prompt, bar, found := strings.Cut(painted, "\r\n")
	if !found || !strings.Contains(prompt, "you> ") {
		t.Fatalf("bar not drawn below the prompt: %q", painted)
	}
	for _, want := range []string{"main", "fake-model", "memory ✓", "alt+⏎ newline"} {
		if !strings.Contains(bar, want) {
			t.Errorf("bar %q is missing %q", bar, want)
		}
	}
	if !strings.Contains(out.String(), "\x1b[1A") {
		t.Errorf("the cursor was not walked back up to the prompt: %q", out.String())
	}

	// An unchanged bar does not repaint.
	out.Reset()
	c.SetBar(Bar{Session: "main", Model: "fake-model", Memory: "memory ✓",
		Hints: []string{"alt+⏎ newline"}})
	if out.String() != "" {
		t.Errorf("repainted an unchanged bar: %q", out.String())
	}

	// A new session name replaces it.
	out.Reset()
	c.SetBar(Bar{Session: "main-2", Model: "fake-model"})
	if !strings.Contains(stripANSI(out.String()), "main-2") {
		t.Errorf("bar not updated: %q", out.String())
	}
	_, _ = w.WriteString("\r")
}

func TestBarFitsTheTerminal(t *testing.T) {
	c, _, _ := rawConsole(t)
	c.color, c.width = true, func() int { return 60 }
	info := Bar{Session: "main", Model: "some/long-model-name", Memory: "memory ✓",
		Hints: []string{"alt+⏎ newline", "↑ history", "/quit"}}
	bar := c.renderBar(info)
	if got := visibleWidth(bar); got != 59 {
		t.Errorf("bar is %d cells wide, want the terminal width less one: %q", got, stripANSI(bar))
	}
	// The hints that do not fit go from the right, so the newline shortcut
	// is the last one standing.
	if !strings.Contains(stripANSI(bar), "alt+⏎ newline") {
		t.Errorf("the newline hint should survive a narrow bar: %q", stripANSI(bar))
	}
	if strings.Contains(stripANSI(bar), "/quit") {
		t.Errorf("the least important hint should be dropped first: %q", stripANSI(bar))
	}
	c.width = func() int { return 30 }
	if narrow := stripANSI(c.renderBar(info)); strings.Contains(narrow, "alt+⏎") || len([]rune(narrow)) != 29 {
		t.Errorf("a very narrow bar = %q", narrow)
	}
	// The bar's own colors resume after each nested style ends.
	if !strings.Contains(bar, ansiReset+c.barStyle) {
		t.Errorf("bar styling not restored after nested styles: %q", bar)
	}
	if c.renderBar(Bar{}) != "" {
		t.Error("an empty Bar should render nothing")
	}
}

func TestBarStyleNamesItsOwnColors(t *testing.T) {
	cases := []struct {
		name              string
		term, colorterm   string
		wantOwnBackground bool
	}{
		{"truecolor", "xterm-ghostty", "truecolor", true},
		{"24bit", "screen", "24bit", true},
		{"256color in TERM", "xterm-256color", "", true},
		{"a terminal that names itself", "xterm-kitty", "", true},
		{"direct color", "xterm-direct", "", true},
		{"a plain terminal", "vt220", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			t.Setenv("COLORTERM", tc.colorterm)
			t.Setenv("NO_COLOR", "")
			got := barStyle()
			// Reverse video would borrow the user's theme and invert it,
			// which is how the bar ended up pale-on-pale.
			if strings.Contains(got, "\x1b[7m") {
				t.Errorf("bar style uses reverse video: %q", got)
			}
			if own := strings.Contains(got, ansiBarBG) && strings.Contains(got, ansiBarFG); own != tc.wantOwnBackground {
				t.Errorf("bar style = %q, want its own background = %v", got, tc.wantOwnBackground)
			}
		})
	}

	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	if got := barStyle(); got != "" {
		t.Errorf("NO_COLOR bar style = %q, want nothing", got)
	}
}

func TestLayoutMapsInputOntoRows(t *testing.T) {
	cases := []struct {
		name                 string
		buf                  string
		cursor, width        int
		rows, curRow, curCol int
	}{
		{"empty", "", 0, 40, 1, 0, 5},
		{"one line", "abc", 3, 40, 1, 0, 8},
		{"cursor at home", "abc", 0, 40, 1, 0, 5},
		{"wrapped", strings.Repeat("x", 40), 40, 40, 2, 1, 5},
		{"newline", "a\nb", 3, 40, 2, 1, 6},
		{"cursor on the newline", "a\nb", 2, 40, 2, 1, 5},
		{"absurd width falls back", "abc", 3, 1, 1, 0, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, curRow, curCol := layout(5, 5, tc.width, []rune(tc.buf), tc.cursor)
			if rows != tc.rows || curRow != tc.curRow || curCol != tc.curCol {
				t.Errorf("layout = %d rows, cursor (%d,%d); want %d rows, cursor (%d,%d)",
					rows, curRow, curCol, tc.rows, tc.curRow, tc.curCol)
			}
		})
	}
}

func TestPrintedTextCannotDriveTheTerminal(t *testing.T) {
	c, w, out := rawConsole(t)
	go func() { _, _ = c.ReadLine() }()
	waitFor(t, func() bool { return strings.Contains(out.String(), "you> ") })

	out.Reset()
	// A reply quoting a scraped page: escape sequences, a stray carriage
	// return, and a bell. None of them may reach the terminal.
	c.Reply("scraped:\x1b[2J\x1b[1;1H\x07 gotcha\rand more", "")
	painted := out.String()
	if strings.Contains(painted, "\x1b[2J") || strings.Contains(painted, "\x07") {
		t.Errorf("control characters survived: %q", painted)
	}
	if !strings.Contains(stripANSI(painted), "scraped:[2J[1;1H gotchaand more") {
		t.Errorf("the visible text was mangled: %q", stripANSI(painted))
	}
	// Newlines are content, not control: multi-line replies still work.
	out.Reset()
	c.Printf("first\x1bsecond\nthird")
	if !strings.Contains(stripANSI(out.String()), "firstsecond\r\nthird") {
		t.Errorf("newlines not kept: %q", stripANSI(out.String()))
	}
	_, _ = w.WriteString("\r")
}

func TestLogWriterPrintsAboveTheLiveRegion(t *testing.T) {
	c, w, out := rawConsole(t)
	go func() { _, _ = c.ReadLine() }()
	if _, err := w.WriteString("typing"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "typing") })

	out.Reset()
	logger := log.New(c.LogWriter(), "", 0)
	logger.Print("WARN memory sidecar restarting\n")
	painted := stripANSI(out.String())
	if !strings.Contains(painted, "WARN memory sidecar restarting") {
		t.Errorf("log line not printed: %q", painted)
	}
	if !strings.Contains(painted, "you> typing") {
		t.Errorf("the prompt was not repainted under the log line: %q", painted)
	}

	// An empty write is not worth a blank line.
	out.Reset()
	if n, err := c.LogWriter().Write([]byte("\n")); n != 1 || err != nil {
		t.Errorf("Write = %d, %v", n, err)
	}
	if out.String() != "" {
		t.Errorf("blank log line painted: %q", out.String())
	}
	_, _ = w.WriteString("\r")
}

func TestSanitizeLeavesOrdinaryTextAlone(t *testing.T) {
	const clean = "plain text — with a tab\tand a newline\n"
	if got := sanitize(clean); got != clean {
		t.Errorf("sanitize changed clean text: %q", got)
	}
}

func TestVisibleWidth(t *testing.T) {
	cases := map[string]int{
		"":                     0,
		"plain":                5,
		"\x1b[36mcolor\x1b[0m": 5,
		"✦✦":                   2,
	}
	for in, want := range cases {
		if got := visibleWidth(in); got != want {
			t.Errorf("visibleWidth(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestTruncateVisible(t *testing.T) {
	if got := truncateVisible("short", 10); got != "short" {
		t.Errorf("untouched = %q", got)
	}
	if got := truncateVisible("short", 0); got != "short" {
		t.Errorf("zero width should be a no-op, got %q", got)
	}
	got := truncateVisible("abcdefgh", 4)
	if got != "abc…"+ansiReset {
		t.Errorf("truncated = %q", got)
	}
	// Escape sequences survive and the styling is closed off.
	got = truncateVisible("\x1b[36mabcdefgh\x1b[0m", 4)
	if !strings.HasPrefix(got, "\x1b[36m") || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("styled truncation = %q", got)
	}
	if visibleWidth(got) > 4 {
		t.Errorf("styled truncation is %d cells wide", visibleWidth(got))
	}
}

// waitFor polls cond until it holds or the test gives up.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// stripANSI drops escape sequences so assertions can read the visible text.
func stripANSI(s string) string {
	var b strings.Builder
	scanANSI(s, func(r rune, visible bool) bool {
		if visible {
			b.WriteRune(r)
		}
		return true
	})
	return b.String()
}
