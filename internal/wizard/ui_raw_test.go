//go:build linux

package wizard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cyqlelabs/factor/internal/config"
)

// The arrow-key menus only run when stdin is a real terminal, so these tests
// give them one: a pty pair whose slave side is the UI's input. Everything
// here is the interactive path a person actually gets.

// syncBuf is the UI's output. Writes are the signal that a keystroke has been
// consumed, which is what lets the driver send the next one without sleeping
// for a fixed time.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type termPair struct {
	master *os.File
	slave  *os.File
	out    *syncBuf
	ui     *UI
}

func newTermPair(t *testing.T) *termPair {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		t.Skipf("cannot unlock pty: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		t.Skipf("cannot name pty: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		t.Skipf("cannot open pty slave: %v", err)
	}
	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
	})

	p := &termPair{master: master, slave: slave, out: &syncBuf{}}
	p.ui = New(slave, p.out)
	if !p.ui.Interactive() {
		t.Fatal("a pty slave was not detected as a terminal")
	}
	return p
}

// send writes one keystroke and waits for the UI to redraw, so the next
// keystroke cannot be swallowed by the same read.
func (p *termPair) send(t *testing.T, keys string) {
	t.Helper()
	before := p.out.Len()
	if _, err := p.master.WriteString(keys); err != nil {
		t.Fatalf("write %q: %v", keys, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.out.Len() == before {
		if time.Now().After(deadline) {
			t.Fatalf("the UI never reacted to %q; output so far:\n%s", keys, p.out.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// commit writes the keystroke a prompt answers by returning, so there is no
// redraw to wait for.
func (p *termPair) commit(t *testing.T, keys string) {
	t.Helper()
	if _, err := p.master.WriteString(keys); err != nil {
		t.Fatalf("write %q: %v", keys, err)
	}
}

// selectAsync runs Select on the pty and returns a channel with its result,
// so the test can drive keystrokes while it is waiting for them.
func (p *termPair) selectAsync(question string, opts []Option, def int) <-chan [2]any {
	done := make(chan [2]any, 1)
	go func() {
		idx, err := p.ui.Select(question, opts, def)
		done <- [2]any{idx, err}
	}()
	return done
}

func await(t *testing.T, done <-chan [2]any) (int, error) {
	t.Helper()
	select {
	case r := <-done:
		idx, _ := r[0].(int)
		err, _ := r[1].(error)
		return idx, err
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt never returned")
		return 0, nil
	}
}

var rawOptions = []Option{{Label: "anthropic", Hint: "claude"}, {Label: "openai"}, {Label: "ollama", Hint: "local"}}

func TestRawSelectArrowKeys(t *testing.T) {
	p := newTermPair(t)
	done := p.selectAsync("provider", rawOptions, 0)
	waitForMenu(t, p)

	p.send(t, "\x1b[B") // down
	p.send(t, "\x1b[B") // down
	p.send(t, "\x1b[A") // up — lands on "openai"
	p.commit(t, "\r")

	idx, err := await(t, done)
	if err != nil || idx != 1 {
		t.Fatalf("selected %d, %v; want openai", idx, err)
	}
	if !contains(p.out.String(), "openai") {
		t.Errorf("the chosen option was not confirmed:\n%s", p.out.String())
	}
}

func TestRawSelectWrapsAndAcceptsVimKeys(t *testing.T) {
	p := newTermPair(t)
	done := p.selectAsync("provider", rawOptions, 0)
	waitForMenu(t, p)

	p.send(t, "k") // up from the first entry wraps to the last
	p.commit(t, "\r")

	idx, err := await(t, done)
	if err != nil || idx != 2 {
		t.Fatalf("selected %d, %v; want the wrapped last entry", idx, err)
	}
}

func TestRawSelectNumberShortcut(t *testing.T) {
	p := newTermPair(t)
	done := p.selectAsync("provider", rawOptions, 0)
	waitForMenu(t, p)

	p.commit(t, "3") // digits pick and commit in one keystroke
	idx, err := await(t, done)
	if err != nil || idx != 2 {
		t.Fatalf("selected %d, %v; want ollama", idx, err)
	}

	// a digit past the end of the menu is ignored, not clamped
	p2 := newTermPair(t)
	done2 := p2.selectAsync("provider", rawOptions, 1)
	waitForMenu(t, p2)
	p2.send(t, "9")
	p2.commit(t, "\r")
	if idx, err := await(t, done2); err != nil || idx != 1 {
		t.Fatalf("selected %d, %v; want the untouched default", idx, err)
	}
}

func TestRawSelectCtrlCAborts(t *testing.T) {
	p := newTermPair(t)
	done := p.selectAsync("provider", rawOptions, 0)
	waitForMenu(t, p)

	p.commit(t, "\x03")
	if _, err := await(t, done); !errors.Is(err, ErrAborted) {
		t.Fatalf("Ctrl-C = %v, want ErrAborted", err)
	}
}

func TestRawSelectAbortsWhenTheTerminalGoesAway(t *testing.T) {
	p := newTermPair(t)
	done := p.selectAsync("provider", rawOptions, 0)
	waitForMenu(t, p)

	_ = p.master.Close() // the far end hangs up mid-prompt
	if _, err := await(t, done); !errors.Is(err, ErrAborted) {
		t.Fatalf("hangup = %v, want ErrAborted", err)
	}
}

func TestRawMultiSelectTogglesWithSpace(t *testing.T) {
	p := newTermPair(t)
	opts := []Option{{Label: "telegram", Hint: "chat"}, {Label: "cli"}, {Label: "http"}}
	done := make(chan struct {
		sel []bool
		err error
	}, 1)
	go func() {
		sel, err := p.ui.MultiSelect("channels", opts, []bool{false, false, false})
		done <- struct {
			sel []bool
			err error
		}{sel, err}
	}()
	waitForMenu(t, p)

	p.send(t, " ")      // toggle telegram on
	p.send(t, "\x1b[B") // down to cli
	p.send(t, "\x1b[B") // down to http
	p.send(t, " ")      // toggle http on
	p.commit(t, "\r")

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.sel[0] || got.sel[1] || !got.sel[2] {
			t.Errorf("selection = %v, want telegram and http", got.sel)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MultiSelect never returned")
	}
	if !contains(p.out.String(), "[x]") {
		t.Errorf("no checked box was ever drawn:\n%s", p.out.String())
	}
}

func TestRawMultiSelectCtrlCAborts(t *testing.T) {
	p := newTermPair(t)
	opts := []Option{{Label: "telegram"}, {Label: "cli"}}
	done := make(chan error, 1)
	go func() {
		_, err := p.ui.MultiSelect("channels", opts, nil)
		done <- err
	}()
	waitForMenu(t, p)

	p.commit(t, "\x04") // Ctrl-D
	select {
	case err := <-done:
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("Ctrl-D = %v, want ErrAborted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MultiSelect never returned")
	}
}

// On a terminal the key is read without echoing it.
func TestRawSecretIsNotEchoed(t *testing.T) {
	p := newTermPair(t)
	done := make(chan string, 1)
	go func() {
		got, err := p.ui.Secret("API key", "")
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	waitForMenu(t, p)

	p.commit(t, "sk-secret\r\n")
	select {
	case got := <-done:
		if got != "sk-secret" {
			t.Errorf("secret = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Secret never returned")
	}
	if contains(p.out.String(), "sk-secret") {
		t.Errorf("the secret was echoed:\n%q", p.out.String())
	}
}

func TestRawSecretKeepsExistingOnBlank(t *testing.T) {
	p := newTermPair(t)
	done := make(chan string, 1)
	go func() {
		got, _ := p.ui.Secret("API key", "sk-old")
		done <- got
	}()
	waitForMenu(t, p)

	p.commit(t, "\r\n")
	select {
	case got := <-done:
		if got != "sk-old" {
			t.Errorf("secret = %q, want the existing one", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Secret never returned")
	}
}

// Run's interactive branch is only taken on a terminal, so it too needs the
// pty: this covers the banner, the "editing the existing config" note and the
// cancellation path a Ctrl-C at the first menu produces.
func TestRunInteractiveCanBeCancelled(t *testing.T) {
	p := newTermPair(t)
	home := tempHome(t)
	path := filepath.Join(home, "config.json")
	if err := config.Default().Save(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), path, Options{Version: "test", Home: home, UI: p.ui})
	}()
	waitForMenu(t, p)

	p.commit(t, "\x03") // Ctrl-C at the provider menu
	select {
	case err := <-done:
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("Run = %v, want ErrAborted", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned")
	}

	out := p.out.String()
	for _, want := range []string{"test", "editing the existing config", "nothing was written"} {
		if !contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// waitForMenu blocks until the prompt has drawn itself, which means it is
// inside its read loop and ready for keystrokes.
func waitForMenu(t *testing.T, p *termPair) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.out.Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the prompt never drew anything")
		}
		time.Sleep(time.Millisecond)
	}
	// let the first full render land before typing into it
	time.Sleep(5 * time.Millisecond)
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
