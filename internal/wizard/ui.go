// Package wizard is Factor's interactive setup: a terminal front-end for
// `factor init` that picks a provider and model (probing them live), installs
// the smrti memory engine, wires up channels, and checks the desktop tools.
//
// The UI is stdlib plus golang.org/x/term: arrow-key menus and masked input
// when stdin is a real terminal, plain numbered prompts when it is a pipe, and
// no ANSI at all when the terminal (or NO_COLOR) says so. Every prompt has a
// default, so holding Enter produces a working configuration.
package wizard

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// ErrAborted is returned when the user interrupts a prompt (Ctrl-C, EOF).
var ErrAborted = errors.New("setup cancelled")

// UI draws prompts and reads answers.
type UI struct {
	in      *os.File // nil when input is not a terminal we can raw-mode
	reader  *bufio.Reader
	out     io.Writer
	color   bool
	rawable bool
}

// New builds a UI for the given streams, auto-detecting terminal features.
func New(in *os.File, out io.Writer) *UI {
	u := &UI{in: in, reader: bufio.NewReader(in), out: out}
	if in != nil && term.IsTerminal(int(in.Fd())) {
		u.rawable = true
	}
	u.color = u.rawable && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	return u
}

// NewPlain builds a non-interactive UI over arbitrary streams (tests, pipes).
func NewPlain(in io.Reader, out io.Writer) *UI {
	return &UI{reader: bufio.NewReader(in), out: out}
}

// Interactive reports whether prompts can use the fancy path.
func (u *UI) Interactive() bool { return u.rawable }

// ---- styling ---------------------------------------------------------------

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiCyan   = "\x1b[36m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
	ansiInvert = "\x1b[7m"
)

func (u *UI) style(code, s string) string {
	if !u.color {
		return s
	}
	return code + s + ansiReset
}

func (u *UI) printf(format string, args ...any) {
	fmt.Fprintf(u.out, format, args...)
}

// Banner prints the header shown once at the start of setup.
func (u *UI) Banner(version string) {
	art := `   ____         __
  / __/__ _____/ /____  ____
 / _// _ '/ __/ __/ _ \/ __/
/_/  \_,_/\__/\__/\___/_/`
	u.printf("\n%s\n", u.style(ansiCyan+ansiBold, art))
	u.printf("  %s\n\n", u.style(ansiDim, "desktop agent with a real memory · "+version))
}

// Step prints a numbered section header.
func (u *UI) Step(n, total int, title string) {
	u.printf("\n%s %s\n", u.style(ansiDim, fmt.Sprintf("[%d/%d]", n, total)), u.style(ansiBold, title))
}

func (u *UI) Info(format string, args ...any) {
	u.printf("      %s\n", fmt.Sprintf(format, args...))
}

func (u *UI) Note(format string, args ...any) {
	u.printf("      %s\n", u.style(ansiDim, fmt.Sprintf(format, args...)))
}

func (u *UI) Success(format string, args ...any) {
	u.printf("  %s   %s\n", u.style(ansiGreen, "✓"), fmt.Sprintf(format, args...))
}

func (u *UI) Warn(format string, args ...any) {
	u.printf("  %s   %s\n", u.style(ansiYellow, "!"), fmt.Sprintf(format, args...))
}

func (u *UI) Fail(format string, args ...any) {
	u.printf("  %s   %s\n", u.style(ansiRed, "✗"), fmt.Sprintf(format, args...))
}

// Summary prints an aligned key/value block.
func (u *UI) Summary(title string, rows [][2]string) {
	u.printf("\n%s\n", u.style(ansiBold, title))
	width := 0
	for _, r := range rows {
		if len(r[0]) > width {
			width = len(r[0])
		}
	}
	for _, r := range rows {
		u.printf("  %s  %s\n", u.style(ansiDim, fmt.Sprintf("%-*s", width, r[0])), r[1])
	}
}

// ---- prompts ---------------------------------------------------------------

// Option is one choice in a Select.
type Option struct {
	Label string
	Hint  string
}

func (u *UI) readLine() (string, error) {
	line, err := u.reader.ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			return "", ErrAborted
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Input asks for a line of text, returning def when the answer is blank.
func (u *UI) Input(question, def string) (string, error) {
	suffix := ""
	if def != "" {
		suffix = u.style(ansiDim, fmt.Sprintf(" [%s]", def))
	}
	u.printf("  %s %s%s ", u.style(ansiCyan, "›"), question, suffix)
	answer, err := u.readLine()
	if err != nil {
		return "", err
	}
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// Secret asks for a value without echoing it. A blank answer keeps current
// (whose presence, not value, is shown).
func (u *UI) Secret(question, current string) (string, error) {
	hint := ""
	if current != "" {
		hint = u.style(ansiDim, " [keep existing]")
	}
	u.printf("  %s %s%s ", u.style(ansiCyan, "›"), question, hint)
	if u.rawable {
		data, err := term.ReadPassword(int(u.in.Fd()))
		u.printf("\n")
		if err != nil {
			return "", err
		}
		answer := strings.TrimSpace(string(data))
		if answer == "" {
			return current, nil
		}
		return answer, nil
	}
	answer, err := u.readLine()
	if err != nil {
		return "", err
	}
	if answer == "" {
		return current, nil
	}
	return answer, nil
}

// Confirm asks a yes/no question.
func (u *UI) Confirm(question string, def bool) (bool, error) {
	choices := "y/N"
	if def {
		choices = "Y/n"
	}
	for {
		u.printf("  %s %s %s ", u.style(ansiCyan, "›"), question, u.style(ansiDim, "["+choices+"]"))
		answer, err := u.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		u.Note("please answer y or n")
	}
}

// Select shows a menu and returns the chosen index. On a real terminal it is
// an arrow-key menu; otherwise a numbered list.
func (u *UI) Select(question string, opts []Option, def int) (int, error) {
	if len(opts) == 0 {
		return 0, fmt.Errorf("no options")
	}
	if def < 0 || def >= len(opts) {
		def = 0
	}
	if u.rawable {
		if idx, err := u.selectRaw(question, opts, def); !errors.Is(err, errNoRawMode) {
			return idx, err
		}
	}
	return u.selectNumbered(question, opts, def)
}

func (u *UI) selectNumbered(question string, opts []Option, def int) (int, error) {
	u.printf("  %s %s\n", u.style(ansiCyan, "›"), question)
	for i, o := range opts {
		line := fmt.Sprintf("      %d) %s", i+1, o.Label)
		if o.Hint != "" {
			line += "  " + u.style(ansiDim, o.Hint)
		}
		u.printf("%s\n", line)
	}
	for {
		u.printf("      %s ", u.style(ansiDim, fmt.Sprintf("choose 1-%d [%d]:", len(opts), def+1)))
		answer, err := u.readLine()
		if err != nil {
			return 0, err
		}
		if answer == "" {
			return def, nil
		}
		n, err := strconv.Atoi(answer)
		if err == nil && n >= 1 && n <= len(opts) {
			return n - 1, nil
		}
		u.Note("enter a number between 1 and %d", len(opts))
	}
}

var errNoRawMode = errors.New("raw mode unavailable")

func (u *UI) selectRaw(question string, opts []Option, def int) (int, error) {
	state, err := term.MakeRaw(int(u.in.Fd()))
	if err != nil {
		return 0, errNoRawMode
	}
	defer func() { _ = term.Restore(int(u.in.Fd()), state) }()

	u.printf("  %s %s %s\r\n", u.style(ansiCyan, "›"), question,
		u.style(ansiDim, "(↑/↓ then Enter)"))
	cursor := def
	u.renderMenu(opts, cursor, false)

	buf := make([]byte, 3)
	for {
		n, err := u.in.Read(buf)
		if err != nil {
			return 0, ErrAborted
		}
		switch {
		case n == 1 && (buf[0] == '\r' || buf[0] == '\n'):
			u.renderMenu(opts, cursor, true)
			return cursor, nil
		case n == 1 && (buf[0] == 3 || buf[0] == 4 || buf[0] == 'q'): // Ctrl-C, Ctrl-D
			u.printf("\r\n")
			return 0, ErrAborted
		case n == 1 && buf[0] >= '1' && buf[0] <= '9':
			if idx := int(buf[0] - '1'); idx < len(opts) {
				cursor = idx
				u.renderMenu(opts, cursor, true)
				return cursor, nil
			}
		case n == 3 && buf[0] == 0x1b && buf[1] == '[':
			switch buf[2] {
			case 'A': // up
				cursor = (cursor - 1 + len(opts)) % len(opts)
			case 'B': // down
				cursor = (cursor + 1) % len(opts)
			}
		case n == 1 && (buf[0] == 'k' || buf[0] == 'j'):
			if buf[0] == 'k' {
				cursor = (cursor - 1 + len(opts)) % len(opts)
			} else {
				cursor = (cursor + 1) % len(opts)
			}
		default:
			continue
		}
		u.moveUp(len(opts))
		u.renderMenu(opts, cursor, false)
	}
}

func (u *UI) renderMenu(opts []Option, cursor int, final bool) {
	for i, o := range opts {
		marker := "  "
		label := o.Label
		if i == cursor {
			marker = u.style(ansiCyan, "❯ ")
			label = u.style(ansiInvert, " "+label+" ")
		}
		line := "    " + marker + label
		if o.Hint != "" {
			line += "  " + u.style(ansiDim, o.Hint)
		}
		u.printf("\x1b[2K%s\r\n", line)
	}
	if final {
		// Collapse the menu to just the chosen line once the user commits.
		u.moveUp(len(opts))
		for range opts {
			u.printf("\x1b[2K\r\n")
		}
		u.moveUp(len(opts))
		u.printf("\x1b[2K    %s %s\r\n", u.style(ansiGreen, "✓"), opts[cursor].Label)
	}
}

func (u *UI) moveUp(n int) {
	if n > 0 {
		u.printf("\x1b[%dA\r", n)
	}
}

// MultiSelect toggles a set of options. Space toggles, Enter accepts.
func (u *UI) MultiSelect(question string, opts []Option, selected []bool) ([]bool, error) {
	if len(selected) != len(opts) {
		selected = make([]bool, len(opts))
	}
	if !u.rawable {
		return u.multiSelectNumbered(question, opts, selected)
	}
	state, err := term.MakeRaw(int(u.in.Fd()))
	if err != nil {
		return u.multiSelectNumbered(question, opts, selected)
	}
	defer func() { _ = term.Restore(int(u.in.Fd()), state) }()

	u.printf("  %s %s %s\r\n", u.style(ansiCyan, "›"), question,
		u.style(ansiDim, "(↑/↓, Space to toggle, Enter to accept)"))
	cursor := 0
	render := func() {
		for i, o := range opts {
			box := "[ ]"
			if selected[i] {
				box = u.style(ansiGreen, "[x]")
			}
			marker := "  "
			if i == cursor {
				marker = u.style(ansiCyan, "❯ ")
			}
			line := fmt.Sprintf("    %s%s %s", marker, box, o.Label)
			if o.Hint != "" {
				line += "  " + u.style(ansiDim, o.Hint)
			}
			u.printf("\x1b[2K%s\r\n", line)
		}
	}
	render()

	buf := make([]byte, 3)
	for {
		n, err := u.in.Read(buf)
		if err != nil {
			return nil, ErrAborted
		}
		switch {
		case n == 1 && (buf[0] == '\r' || buf[0] == '\n'):
			return selected, nil
		case n == 1 && (buf[0] == 3 || buf[0] == 4):
			u.printf("\r\n")
			return nil, ErrAborted
		case n == 1 && buf[0] == ' ':
			selected[cursor] = !selected[cursor]
		case n == 3 && buf[0] == 0x1b && buf[1] == '[':
			switch buf[2] {
			case 'A':
				cursor = (cursor - 1 + len(opts)) % len(opts)
			case 'B':
				cursor = (cursor + 1) % len(opts)
			}
		default:
			continue
		}
		u.moveUp(len(opts))
		render()
	}
}

func (u *UI) multiSelectNumbered(question string, opts []Option, selected []bool) ([]bool, error) {
	u.printf("  %s %s\n", u.style(ansiCyan, "›"), question)
	var preset []string
	for i, o := range opts {
		mark := " "
		if selected[i] {
			mark = "x"
			preset = append(preset, strconv.Itoa(i+1))
		}
		u.printf("      [%s] %d) %s  %s\n", mark, i+1, o.Label, u.style(ansiDim, o.Hint))
	}
	u.printf("      %s ", u.style(ansiDim, fmt.Sprintf("numbers, comma-separated [%s]:", strings.Join(preset, ","))))
	answer, err := u.readLine()
	if err != nil {
		return nil, err
	}
	if answer == "" {
		return selected, nil
	}
	out := make([]bool, len(opts))
	if strings.EqualFold(answer, "none") {
		return out, nil
	}
	for _, part := range strings.Split(answer, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && n >= 1 && n <= len(opts) {
			out[n-1] = true
		}
	}
	return out, nil
}

// ---- progress --------------------------------------------------------------

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Task runs fn while showing a spinner, then reports the outcome. The
// returned error is fn's, so callers can branch on it.
func (u *UI) Task(label string, fn func() error) error {
	if !u.color {
		u.printf("      %s… ", label)
		err := fn()
		if err != nil {
			u.printf("failed: %v\n", err)
		} else {
			u.printf("ok\n")
		}
		return err
	}

	done := make(chan error, 1)
	go func() { done <- fn() }()

	ticker := time.NewTicker(90 * time.Millisecond)
	defer ticker.Stop()
	frame := 0
	for {
		select {
		case err := <-done:
			u.printf("\r\x1b[2K")
			if err != nil {
				u.Fail("%s — %v", label, err)
			} else {
				u.Success("%s", label)
			}
			return err
		case <-ticker.C:
			u.printf("\r\x1b[2K  %s   %s", u.style(ansiCyan, spinnerFrames[frame%len(spinnerFrames)]), label)
			frame++
		}
	}
}

// Progress returns a function that prints sub-steps of a long task (used by
// the smrti installer, which reports which installer it is trying).
func (u *UI) Progress() func(string, ...any) {
	var mu sync.Mutex
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		u.printf("\r\x1b[2K      %s\n", u.style(ansiDim, fmt.Sprintf(format, args...)))
	}
}
