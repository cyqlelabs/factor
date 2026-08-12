//go:build linux

package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY hands back both ends of a fresh pseudo-terminal: writes to the
// master are keystrokes, reads from it are what the console painted.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("cannot unlock the pty: %v", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("cannot name the pty: %v", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open the pty slave: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = m.Close() })
	return m, s
}

func TestConsoleDrivesARealTerminal(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	master, slave := openPTY(t)
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Col: 62, Row: 24}); err != nil {
		t.Skipf("cannot size the pty: %v", err)
	}

	painted := &syncBuf{}
	go func() { _, _ = io.Copy(painted, master) }()

	c := NewChat(slave, slave)
	if c.Interactive() {
		t.Fatal("the console should not paint before Start")
	}
	c.Start()
	if !c.Interactive() {
		t.Fatal("a real terminal should switch on the live region")
	}
	if got := c.width(); got != 62 {
		t.Errorf("width = %d, want the terminal's 62 columns", got)
	}

	c.SetStatus("  ~~~ thinking")
	if _, err := master.WriteString("hello\r"); err != nil {
		t.Fatal(err)
	}
	line, err := c.ReadLine()
	if err != nil || line != "hello" {
		t.Fatalf("ReadLine = %q, %v", line, err)
	}
	c.Reply("hi back", "1.0s")
	c.Close()

	for _, want := range []string{"~~~ thinking", "hello", "hi back", "1.0s"} {
		waitFor(t, func() bool { return strings.Contains(stripANSI(painted.String()), want) })
	}
	if !strings.Contains(painted.String(), "\x1b[36m") {
		t.Errorf("color was not used on a color terminal: %q", painted.String())
	}
}

func TestNewStatusUsesTheTerminalWidth(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	master, slave := openPTY(t)
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Col: 30, Row: 24}); err != nil {
		t.Skipf("cannot size the pty: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, master) }()

	c := NewStatus(slave)
	if !c.Interactive() {
		t.Fatal("a status console on a terminal should paint")
	}
	if got := c.width(); got != 30 {
		t.Errorf("width = %d, want 30", got)
	}
	c.SetStatus("working")
	c.Close()
}

func TestNoColorAndDumbTerminalsStayPlain(t *testing.T) {
	master, slave := openPTY(t)
	go func() { _, _ = io.Copy(io.Discard, master) }()

	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	if c := NewChat(slave, slave); c.color || c.canRaw {
		t.Errorf("NO_COLOR should keep the console plain (color=%v raw=%v)", c.color, c.canRaw)
	}

	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "")
	c := NewChat(slave, slave)
	if c.color || c.canRaw {
		t.Errorf("TERM=dumb should keep the console plain (color=%v raw=%v)", c.color, c.canRaw)
	}
	c.Start()
	if c.Interactive() {
		t.Error("Start should not switch on painting for a dumb terminal")
	}
	if got := c.width(); got != defaultWidth {
		t.Errorf("width = %d, want the default %d", got, defaultWidth)
	}
}
