// Package tui draws Factor's interactive chat.
//
// On a real terminal the console owns the bottom rows of the screen: an
// activity line showing what the agent is doing right now, and below it a
// prompt line that stays editable while a turn runs. Replies, background
// job results, and status repaints all land above the prompt, so nothing
// ever overwrites what you are half-way through typing — and a message you
// send never looks unanswered.
//
// Without a terminal (pipes, `TERM=dumb`, no raw mode) every method falls
// back to plain lines, so scripts and tests see ordinary output.
package tui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/term"
)

// ErrInterrupted is returned by ReadLine when Ctrl-C is pressed on an empty
// line: the REPL's cue to quit.
var ErrInterrupted = errors.New("interrupted")

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiCyan    = "\x1b[36m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiMagenta = "\x1b[35m"
)

const defaultWidth = 80

// Console renders the chat and reads the user's input.
type Console struct {
	in     *os.File
	out    io.Writer
	reader *bufio.Reader
	width  func() int

	canRaw bool
	color  bool

	// pending holds bytes read but not yet turned into keys — type-ahead
	// and pasted lines that arrived past an Enter. Only the reader touches
	// it, and there is exactly one reader.
	pending []byte

	mu       sync.Mutex
	paint    bool // the live region is ours to repaint
	rawInput bool // keystrokes are ours to interpret
	state    *term.State
	prompt   string
	buf      []rune
	cursor   int
	status   string
	above    int // physical rows between the live region's top and the cursor
	drawn    bool
	hist     []string
	histIdx  int
	stash    string
}

// NewChat builds the console for the interactive REPL. Painting and line
// editing switch on in Start, once the terminal is actually in raw mode.
func NewChat(in, out *os.File) *Console {
	c := newConsole(out)
	c.in = in
	if in != nil {
		c.reader = bufio.NewReader(in)
		c.canRaw = c.color && term.IsTerminal(int(in.Fd()))
	}
	c.prompt = c.style(ansiCyan+ansiBold, "you") + c.style(ansiDim, "› ")
	return c
}

// NewStatus builds a console that only draws the activity line — one-shot
// mode, where there is no prompt and nothing to read.
func NewStatus(out *os.File) *Console {
	c := newConsole(out)
	c.mu.Lock()
	c.paint = c.color
	c.mu.Unlock()
	return c
}

func newConsole(out *os.File) *Console {
	c := &Console{out: io.Discard, width: func() int { return defaultWidth }}
	if out == nil {
		return c
	}
	c.out = out
	usable := os.Getenv("TERM") != "dumb" && term.IsTerminal(int(out.Fd()))
	c.color = usable && os.Getenv("NO_COLOR") == ""
	if usable {
		c.width = func() int {
			if w, _, err := term.GetSize(int(out.Fd())); err == nil && w >= 24 {
				return w
			}
			return defaultWidth
		}
	}
	return c
}

// Start hands the terminal to the console: raw mode, so it owns keystrokes
// and can repaint the bottom rows. Anything less than a real terminal leaves
// the console in its plain, line-at-a-time mode.
func (c *Console) Start() {
	if !c.canRaw {
		return
	}
	state, err := term.MakeRaw(int(c.in.Fd()))
	if err != nil {
		return
	}
	c.mu.Lock()
	c.state, c.rawInput, c.paint = state, true, true
	c.mu.Unlock()
}

// Close erases the live region and gives the terminal back.
func (c *Console) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = ""
	c.eraseLocked()
	if c.state != nil {
		_ = term.Restore(int(c.in.Fd()), c.state)
		c.state, c.rawInput, c.paint = nil, false, false
	}
}

// Interactive reports whether the console is drawing a live region.
func (c *Console) Interactive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paint
}

// ---- styling ---------------------------------------------------------------

func (c *Console) style(code, s string) string {
	if !c.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

// ---- output ----------------------------------------------------------------

// Printf prints a line above the live region.
func (c *Console) Printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printLocked(sanitize(fmt.Sprintf(format, args...)))
}

// Reply prints an agent message. meta is a dim trailing note (how long the
// turn took, which tools it used); an empty meta is left out.
func (c *Console) Reply(content, meta string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	text := c.style(ansiCyan+ansiBold, "factor") + c.style(ansiDim, "› ") + sanitize(content)
	if meta != "" {
		text += "\n" + c.style(ansiDim, "  "+meta)
	}
	c.printLocked(text)
}

// LogWriter returns a writer that prints log lines above the live region.
// Point the standard logger at it (`log.SetOutput`) so a background warning
// cannot land in the middle of the prompt and desync the layout.
func (c *Console) LogWriter() io.Writer { return logWriter{c} }

type logWriter struct{ con *Console }

func (w logWriter) Write(p []byte) (int, error) {
	text := sanitize(strings.TrimRight(string(p), "\n"))
	if text == "" {
		return len(p), nil
	}
	w.con.mu.Lock()
	defer w.con.mu.Unlock()
	w.con.printLocked(w.con.style(ansiDim, text))
	return len(p), nil
}

// SetStatus replaces the activity line; an empty status removes it.
func (c *Console) SetStatus(status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paint || status == c.status {
		return
	}
	c.status = status
	c.eraseLocked()
	c.drawLocked()
}

// PromptIdle switches the prompt back to the resting one.
func (c *Console) PromptIdle() {
	c.setPrompt(c.style(ansiCyan+ansiBold, "you") + c.style(ansiDim, "› "))
}

// PromptSteering marks the prompt while a turn is in flight: whatever is
// typed now is steered into the running turn rather than starting a new one.
func (c *Console) PromptSteering() {
	c.setPrompt(c.style(ansiYellow+ansiBold, "steer") + c.style(ansiDim, "› "))
}

func (c *Console) setPrompt(prompt string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prompt == c.prompt {
		return
	}
	c.prompt = prompt
	if c.rawInput {
		c.eraseLocked()
		c.drawLocked()
	}
}

func (c *Console) writeLocked(s string) {
	if s != "" {
		_, _ = io.WriteString(c.out, s)
	}
}

func (c *Console) printLocked(text string) {
	if !c.paint {
		c.writeLocked(text + "\n")
		return
	}
	c.eraseLocked()
	if c.rawInput {
		text = strings.ReplaceAll(text, "\n", "\r\n") + "\r\n"
	} else {
		text += "\n"
	}
	c.writeLocked(text)
	c.drawLocked()
}

// eraseLocked walks back to the top of the live region and clears everything
// from there down.
func (c *Console) eraseLocked() {
	if !c.paint || !c.drawn {
		return
	}
	var b strings.Builder
	if c.above > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", c.above)
	}
	b.WriteString("\r\x1b[J")
	c.writeLocked(b.String())
	c.above, c.drawn = 0, false
}

// drawLocked paints the activity line and, in raw mode, the prompt line
// under it — leaving the cursor exactly where the user is typing. The
// wrap math counts one cell per rune, so a line of double-width glyphs
// (CJK, emoji) can wrap a row earlier than it thinks; Ctrl-L repaints.
func (c *Console) drawLocked() {
	if !c.paint {
		return
	}
	width := c.width()
	var b strings.Builder
	above := 0
	if c.status != "" {
		b.WriteString(truncateVisible(c.status, width-1))
		if c.rawInput {
			b.WriteString("\r\n")
			above++
		}
	}
	if c.rawInput {
		b.WriteString(c.prompt)
		b.WriteString(string(c.buf))
		promptWidth := visibleWidth(c.prompt)
		endRow := (promptWidth + len(c.buf)) / width
		curRow, curCol := (promptWidth+c.cursor)/width, (promptWidth+c.cursor)%width
		if endRow > curRow {
			fmt.Fprintf(&b, "\x1b[%dA", endRow-curRow)
		}
		b.WriteString("\r")
		if curCol > 0 {
			fmt.Fprintf(&b, "\x1b[%dC", curCol)
		}
		above += curRow
	}
	c.writeLocked(b.String())
	c.above, c.drawn = above, true
}

// ---- input -----------------------------------------------------------------

// ReadLine returns the next line the user submits. In raw mode the prompt
// stays live while it waits: activity updates and printed messages repaint
// around half-typed text instead of clobbering it.
func (c *Console) ReadLine() (string, error) {
	c.mu.Lock()
	raw := c.rawInput
	c.mu.Unlock()
	if raw {
		return c.readLineRaw()
	}
	return c.readLinePlain()
}

func (c *Console) readLinePlain() (string, error) {
	if c.reader == nil {
		return "", io.EOF
	}
	c.mu.Lock()
	c.writeLocked(c.prompt)
	c.mu.Unlock()

	line, err := c.reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *Console) readLineRaw() (string, error) {
	c.mu.Lock()
	c.buf, c.cursor = c.buf[:0], 0
	c.eraseLocked()
	c.drawLocked()
	c.mu.Unlock()

	chunk := make([]byte, 128)
	for {
		// Keys typed or pasted past the last Enter are waiting in c.pending.
		for len(c.pending) > 0 {
			size, k := decodeKey(c.pending)
			if size == 0 {
				break // an incomplete sequence: wait for the rest
			}
			c.pending = c.pending[size:]
			line, done, kerr := c.apply(k)
			if done {
				return line, kerr
			}
		}
		n, err := c.in.Read(chunk)
		c.pending = append(c.pending, chunk[:n]...)
		if err != nil && n == 0 {
			c.mu.Lock()
			c.eraseLocked()
			c.mu.Unlock()
			if errors.Is(err, io.EOF) {
				return "", io.EOF
			}
			return "", err
		}
	}
}

// apply folds one keypress into the line buffer. done marks the line (or the
// session) as finished.
func (c *Console) apply(k key) (line string, done bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch k.kind {
	case keyEnter:
		line = string(c.buf)
		c.pushHistory(line)
		c.buf, c.cursor = c.buf[:0], 0
		c.eraseLocked()
		c.writeLocked(c.prompt + line + "\r\n")
		c.drawLocked()
		return line, true, nil
	case keyInterrupt:
		if len(c.buf) > 0 {
			c.buf, c.cursor = c.buf[:0], 0
			break
		}
		c.eraseLocked()
		c.writeLocked(c.prompt + c.style(ansiDim, "^C") + "\r\n")
		return "", true, ErrInterrupted
	case keyEOF:
		if len(c.buf) == 0 {
			c.eraseLocked()
			return "", true, io.EOF
		}
		c.deleteForward()
	case keyRune:
		c.buf = append(c.buf, 0)
		copy(c.buf[c.cursor+1:], c.buf[c.cursor:])
		c.buf[c.cursor] = k.r
		c.cursor++
	case keyBackspace:
		if c.cursor > 0 {
			c.buf = append(c.buf[:c.cursor-1], c.buf[c.cursor:]...)
			c.cursor--
		}
	case keyDelete:
		c.deleteForward()
	case keyLeft:
		if c.cursor > 0 {
			c.cursor--
		}
	case keyRight:
		if c.cursor < len(c.buf) {
			c.cursor++
		}
	case keyHome:
		c.cursor = 0
	case keyEnd:
		c.cursor = len(c.buf)
	case keyKillLine:
		c.buf = c.buf[:c.cursor]
	case keyClearLine:
		c.buf = append(c.buf[:0], c.buf[c.cursor:]...)
		c.cursor = 0
	case keyKillWord:
		c.killWord()
	case keyUp:
		c.recall(-1)
	case keyDown:
		c.recall(1)
	case keyClearScreen:
		c.drawn = false
		c.writeLocked("\x1b[2J\x1b[H")
	case keyIgnore:
		return "", false, nil
	}

	c.eraseLocked()
	c.drawLocked()
	return "", false, nil
}

func (c *Console) deleteForward() {
	if c.cursor < len(c.buf) {
		c.buf = append(c.buf[:c.cursor], c.buf[c.cursor+1:]...)
	}
}

func (c *Console) killWord() {
	end := c.cursor
	for end > 0 && c.buf[end-1] == ' ' {
		end--
	}
	for end > 0 && c.buf[end-1] != ' ' {
		end--
	}
	c.buf = append(c.buf[:end], c.buf[c.cursor:]...)
	c.cursor = end
}

func (c *Console) pushHistory(line string) {
	if strings.TrimSpace(line) != "" && (len(c.hist) == 0 || c.hist[len(c.hist)-1] != line) {
		c.hist = append(c.hist, line)
	}
	c.histIdx, c.stash = len(c.hist), ""
}

// recall walks the history: -1 towards older entries, +1 back towards the
// line that was being typed when the walk started.
func (c *Console) recall(delta int) {
	idx := c.histIdx + delta
	if idx < 0 || idx > len(c.hist) {
		return
	}
	if c.histIdx == len(c.hist) {
		c.stash = string(c.buf)
	}
	c.histIdx = idx
	if idx == len(c.hist) {
		c.buf = []rune(c.stash)
	} else {
		c.buf = []rune(c.hist[idx])
	}
	c.cursor = len(c.buf)
}

// ---- width helpers ---------------------------------------------------------

// sanitize drops the control characters printed text has no business
// carrying. A reply quoting a scraped page or a file can contain escape
// sequences; left alone they would move the cursor around inside the live
// region — whose bookkeeping assumes printed text only ever moves it down.
func sanitize(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
}

func isControl(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f
}

// scanANSI walks s rune by rune, reporting whether each one takes up a cell
// or is part of an escape sequence. fn stops the walk by returning false.
func scanANSI(s string, fn func(r rune, visible bool) bool) {
	const (
		plain = iota
		afterEscape
		insideCSI
	)
	state := plain
	for _, r := range s {
		switch state {
		case afterEscape:
			// Only CSI ("\x1b[…") keeps running; anything else ends here.
			state = plain
			if r == '[' {
				state = insideCSI
			}
		case insideCSI:
			if r >= 0x40 && r <= 0x7e { // the final byte of the sequence
				state = plain
			}
		default:
			if r == 0x1b {
				state = afterEscape
				break
			}
			if !fn(r, true) {
				return
			}
			continue
		}
		if !fn(r, false) {
			return
		}
	}
}

// visibleWidth is the cell width of s, skipping ANSI escape sequences.
func visibleWidth(s string) int {
	width := 0
	scanANSI(s, func(_ rune, visible bool) bool {
		if visible {
			width++
		}
		return true
	})
	return width
}

// truncateVisible cuts s to at most max visible cells, keeping escape
// sequences intact and closing any styling it cut through.
func truncateVisible(s string, max int) string {
	if max <= 0 || visibleWidth(s) <= max {
		return s
	}
	var b strings.Builder
	width := 0
	scanANSI(s, func(r rune, visible bool) bool {
		if !visible {
			b.WriteRune(r)
			return true
		}
		if width == max-1 {
			return false
		}
		b.WriteRune(r)
		width++
		return true
	})
	b.WriteString("…" + ansiReset)
	return b.String()
}

// ---- keys ------------------------------------------------------------------

type keyKind int

const (
	keyIgnore keyKind = iota
	keyRune
	keyEnter
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyUp
	keyDown
	keyKillLine
	keyKillWord
	keyClearLine
	keyClearScreen
	keyInterrupt
	keyEOF
)

type key struct {
	kind keyKind
	r    rune
}

// decodeKey pulls the next keypress out of b and reports how many bytes it
// consumed. Zero means b ends mid-sequence and more input is needed.
func decodeKey(b []byte) (int, key) {
	if len(b) == 0 {
		return 0, key{}
	}
	switch b[0] {
	case 0x1b:
		return decodeEscape(b)
	case '\r', '\n':
		return 1, key{kind: keyEnter}
	case 0x7f, 0x08:
		return 1, key{kind: keyBackspace}
	case 0x01:
		return 1, key{kind: keyHome}
	case 0x02:
		return 1, key{kind: keyLeft}
	case 0x03:
		return 1, key{kind: keyInterrupt}
	case 0x04:
		return 1, key{kind: keyEOF}
	case 0x05:
		return 1, key{kind: keyEnd}
	case 0x06:
		return 1, key{kind: keyRight}
	case 0x0b:
		return 1, key{kind: keyKillLine}
	case 0x0c:
		return 1, key{kind: keyClearScreen}
	case 0x15:
		return 1, key{kind: keyClearLine}
	case 0x17:
		return 1, key{kind: keyKillWord}
	}
	if b[0] < 0x20 {
		return 1, key{kind: keyIgnore}
	}
	if !utf8.FullRune(b) && len(b) < utf8.UTFMax {
		return 0, key{}
	}
	r, size := utf8.DecodeRune(b)
	if r == utf8.RuneError && size <= 1 {
		return 1, key{kind: keyIgnore}
	}
	return size, key{kind: keyRune, r: r}
}

func decodeEscape(b []byte) (int, key) {
	if len(b) < 3 {
		return 0, key{}
	}
	if b[1] != '[' && b[1] != 'O' {
		return 2, key{kind: keyIgnore}
	}
	switch b[2] {
	case 'A':
		return 3, key{kind: keyUp}
	case 'B':
		return 3, key{kind: keyDown}
	case 'C':
		return 3, key{kind: keyRight}
	case 'D':
		return 3, key{kind: keyLeft}
	case 'H':
		return 3, key{kind: keyHome}
	case 'F':
		return 3, key{kind: keyEnd}
	}
	if b[1] == '[' {
		// A longer CSI sequence: consume through its final byte.
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				if b[i] == '~' && i == 3 && b[2] == '3' {
					return 4, key{kind: keyDelete}
				}
				return i + 1, key{kind: keyIgnore}
			}
		}
		return 0, key{}
	}
	return 3, key{kind: keyIgnore}
}
