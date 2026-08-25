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
	ansiBarBG   = "\x1b[48;5;236m"
	ansiBarFG   = "\x1b[38;5;250m"
	ansiBlue    = "\x1b[34m"
	// The closest 256-color match to the Factor logo's blue (#0b94d4).
	ansiLogoBlue = "\x1b[38;5;32m"
)

const defaultWidth = 80

// Console renders the chat and reads the user's input.
type Console struct {
	in     *os.File
	out    io.Writer
	reader *bufio.Reader
	width  func() int

	canRaw   bool
	color    bool
	barStyle string
	blue     string

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
	bar      string
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
	usable := os.Getenv("TERM") != "dumb" && term.IsTerminal(int(out.Fd())) && EnableVT(out)
	c.color = usable && os.Getenv("NO_COLOR") == ""
	c.barStyle = barStyle()
	c.blue = blueStyle()
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
	text := c.style(c.blue+ansiBold, "factor") + c.style(ansiDim, "› ") + sanitize(content)
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

// drawLocked paints the live region — activity line, the (possibly
// multi-line) input, and the status bar under it — leaving the cursor
// exactly where the user is typing. The wrap math counts one cell per rune,
// so a line of double-width glyphs (CJK, emoji) can wrap a row earlier than
// it thinks; Ctrl-L repaints.
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
		indent := c.indent()
		b.WriteString(renderInput(c.prompt, indent, c.buf))
		rows, curRow, curCol := layout(visibleWidth(c.prompt), visibleWidth(indent), width, c.buf, c.cursor)
		below := rows - 1 - curRow
		if c.bar != "" {
			b.WriteString("\r\n")
			b.WriteString(truncateVisible(c.bar, width-1))
			below++
		}
		if below > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", below)
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

// indent is the guide drawn at the head of every continuation row, sized to
// line the text up under the prompt.
func (c *Console) indent() string {
	pad := visibleWidth(c.prompt) - 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + c.style(ansiDim, "┆ ")
}

// renderInput writes the prompt, the buffer, and a continuation guide at the
// head of each row the user broke with a newline.
func renderInput(prompt, indent string, buf []rune) string {
	var b strings.Builder
	b.WriteString(prompt)
	for _, r := range buf {
		if r == '\n' {
			b.WriteString("\r\n")
			b.WriteString(indent)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// layout maps the prompt and buffer onto physical rows, reporting how many
// rows they cover and where the cursor lands inside them.
func layout(promptWidth, indentWidth, width int, buf []rune, cursor int) (rows, curRow, curCol int) {
	if width < 2 {
		width = defaultWidth
	}
	row, col := 0, promptWidth
	if cursor == 0 {
		curRow, curCol = row, col
	}
	for i, r := range buf {
		if r == '\n' {
			row, col = row+1, indentWidth
		} else if col++; col >= width {
			row, col = row+1, 0
		}
		if i+1 == cursor {
			curRow, curCol = row, col
		}
	}
	return row + 1, curRow, curCol
}

// ---- status bar ------------------------------------------------------------

// Bar is the persistent bottom line: which conversation you are in, what is
// answering it, and the keys that drive the prompt.
type Bar struct {
	Session string
	Model   string
	Cost    string // what this session has spent so far; "" leaves it out
	Memory  string // "" leaves memory out entirely
	// Voice is the microphone/speech meter ("" leaves it out); VoiceTone
	// picks its color — "hear" while the mic carries speech, "speak" while
	// the agent talks, "warn" for a dead microphone, anything else plain.
	Voice     string
	VoiceTone string
	Hints     []string // right-aligned, dropped first when the terminal is narrow
}

// voiceToneStyle maps a meter tone onto the bar's palette.
func voiceToneStyle(tone string) string {
	switch tone {
	case "hear":
		return ansiGreen
	case "speak":
		return ansiCyan
	case "warn":
		return ansiYellow
	}
	return ""
}

// SetBar replaces the status bar. A zero Bar removes it.
func (c *Console) SetBar(bar Bar) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rendered := c.renderBar(bar)
	if !c.paint || !c.rawInput || rendered == c.bar {
		return
	}
	c.bar = rendered
	c.eraseLocked()
	c.drawLocked()
}

func (c *Console) renderBar(bar Bar) string {
	left := make([]string, 0, 3)
	if bar.Session != "" {
		left = append(left, c.style(ansiBold, bar.Session))
	}
	for _, part := range []string{bar.Model, bar.Cost, bar.Memory} {
		if part != "" {
			left = append(left, part)
		}
	}
	if bar.Voice != "" {
		if code := voiceToneStyle(bar.VoiceTone); code != "" {
			left = append(left, c.style(code, bar.Voice))
		} else {
			left = append(left, bar.Voice)
		}
	}
	if len(left) == 0 && len(bar.Hints) == 0 {
		return ""
	}
	text := " " + strings.Join(left, c.style(ansiDim, " · "))
	width := c.width() - 1
	// Hints are right-aligned, and the least important ones (last) go first
	// when the terminal is too narrow to hold them all.
	for n := len(bar.Hints); n > 0; n-- {
		hints := strings.Join(bar.Hints[:n], " · ")
		if gap := width - visibleWidth(text) - visibleWidth(hints) - 1; gap > 0 {
			text += strings.Repeat(" ", gap) + hints
			break
		}
	}
	if pad := width - visibleWidth(text); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return c.styleBar(truncateVisible(text, width))
}

// barStyle picks how the bar is painted. A terminal with a real palette gets
// a bar that names both its background and its foreground, so it reads the
// same under any theme; everything else gets plain dim text. Reverse video
// is deliberately not used: it borrows the theme's colors and inverts them,
// which is how a bar ends up as pale text on a pale background.
func barStyle() string {
	if os.Getenv("NO_COLOR") != "" {
		return ""
	}
	if paletteTerm() {
		return ansiBarBG + ansiBarFG
	}
	return ansiDim
}

// blueStyle is the logo's blue on terminals with a real palette, and the
// theme's plain blue everywhere else.
func blueStyle() string {
	if paletteTerm() {
		return ansiLogoBlue
	}
	return ansiBlue
}

// paletteTerm reports whether the terminal has a real 256-color palette.
func paletteTerm() bool {
	switch os.Getenv("COLORTERM") {
	case "truecolor", "24bit":
		return true
	}
	// TERM names the terminal, not its palette: xterm-ghostty and friends
	// support 256 colors without saying so.
	term := os.Getenv("TERM")
	for _, name := range []string{"256color", "direct", "kitty", "ghostty",
		"alacritty", "wezterm", "foot", "contour", "rio", "iterm"} {
		if strings.Contains(term, name) {
			return true
		}
	}
	return false
}

// styleBar paints the whole row. Nested styling inside the text resets as it
// ends, so the bar's own colors are re-applied after every reset it finds.
func (c *Console) styleBar(text string) string {
	if !c.color {
		return text
	}
	return c.barStyle + strings.ReplaceAll(text, ansiReset, ansiReset+c.barStyle) + ansiReset
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
		// A trailing backslash continues the message instead of sending it —
		// the shortcut that works on terminals which swallow Alt-Enter.
		if c.cursor == len(c.buf) && len(c.buf) > 0 && c.buf[len(c.buf)-1] == '\\' {
			c.buf[len(c.buf)-1] = '\n'
			break
		}
		line = string(c.buf)
		c.pushHistory(line)
		c.buf, c.cursor = c.buf[:0], 0
		c.eraseLocked()
		c.writeLocked(renderInput(c.prompt, c.indent(), []rune(line)) + "\r\n")
		c.drawLocked()
		return line, true, nil
	case keyNewline:
		c.insert('\n')
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
		c.insert(k.r)
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

func (c *Console) insert(r rune) {
	c.buf = append(c.buf, 0)
	copy(c.buf[c.cursor+1:], c.buf[c.cursor:])
	c.buf[c.cursor] = r
	c.cursor++
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
	keyNewline
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
	case '\r':
		return 1, key{kind: keyEnter}
	case '\n': // Ctrl-J: a newline inside the message
		return 1, key{kind: keyNewline}
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
	// Alt-Enter is only two bytes: decide it before waiting for a third,
	// or the newline would not appear until the next keystroke.
	if len(b) >= 2 && (b[1] == '\r' || b[1] == '\n') {
		return 2, key{kind: keyNewline}
	}
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
			if b[i] < 0x40 || b[i] > 0x7e {
				continue
			}
			seq := string(b[2 : i+1])
			if seq == "3~" {
				return 4, key{kind: keyDelete}
			}
			// Shift-Enter, from terminals that disambiguate it: kitty's
			// CSI 13;<mods> u or xterm's CSI 27;<mods>;13 ~.
			if strings.HasPrefix(seq, "13;") && b[i] == 'u' ||
				strings.HasPrefix(seq, "27;") && strings.HasSuffix(seq, ";13~") {
				return i + 1, key{kind: keyNewline}
			}
			return i + 1, key{kind: keyIgnore}
		}
		return 0, key{}
	}
	return 3, key{kind: keyIgnore}
}
