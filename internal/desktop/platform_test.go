package desktop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The macOS and Windows controllers shell out to osascript and PowerShell, so
// their scripts can be built and asserted on any platform — which is exactly
// what these tests do. Behaviour on the real OS still depends on the host,
// but the argument construction, escaping and parsing are covered everywhere.

func TestMacListParsing(t *testing.T) {
	m := newMachine("darwin", "osascript")
	m.outputs["osascript"] = "501:1\t501\tSafari\t0\t25\t1440\t875\tFactor — Safari\n" +
		"640:2\t640\tTerminal\t100\t120\t800\t600\t~/factor\n"
	c := NewController(m.Env())

	wins, err := c.ListWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(wins) != 2 || wins[0].ID != "501:1" || wins[0].App != "Safari" || wins[0].W != 1440 {
		t.Fatalf("windows = %+v", wins)
	}
	if wins[1].Title != "~/factor" {
		t.Errorf("title = %q", wins[1].Title)
	}
}

func TestMacWindowIDValidation(t *testing.T) {
	m := newMachine("darwin", "osascript")
	c := NewController(m.Env())
	err := c.Focus(context.Background(), Window{ID: "0x1234"})
	if err == nil || !strings.Contains(err.Error(), "pid:index") {
		t.Fatalf("error = %v", err)
	}
}

func TestMacKeyTranslation(t *testing.T) {
	m := newMachine("darwin", "osascript")
	c := NewController(m.Env())
	ctx := context.Background()

	if err := c.PressKey(ctx, "cmd+shift+t", 1); err != nil {
		t.Fatal(err)
	}
	script := m.lastCall()[2]
	if !strings.Contains(script, `keystroke "t" using {command down, shift down}`) {
		t.Errorf("script = %s", script)
	}

	m.calls = nil
	if err := c.PressKey(ctx, "Return", 2); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 2 {
		t.Errorf("repeat=2 ran %d scripts", len(m.calls))
	}
	if !strings.Contains(m.lastCall()[2], "key code 36") {
		t.Errorf("script = %s", m.lastCall()[2])
	}
	if err := c.PressKey(ctx, "hyper+x", 1); err == nil {
		t.Error("unknown modifier accepted")
	}
	if err := c.PressKey(ctx, "notakey", 1); err == nil {
		t.Error("unknown key accepted")
	}
}

func TestMacAppleScriptEscaping(t *testing.T) {
	m := newMachine("darwin", "osascript")
	c := NewController(m.Env())
	if err := c.TypeText(context.Background(), `say "hi" \ bye`, 0); err != nil {
		t.Fatal(err)
	}
	script := m.lastCall()[2]
	if !strings.Contains(script, `keystroke "say \"hi\" \\ bye"`) {
		t.Errorf("escaping is wrong: %s", script)
	}
}

func TestMacScreenshotRegion(t *testing.T) {
	m := newMachine("darwin", "screencapture")
	c := NewController(m.Env())
	err := c.Screenshot(context.Background(), "/tmp/x.png", Shot{
		Mode:   "region",
		Region: Geometry{X: 1, Y: 2, W: 30, H: 40, HasPos: true, HasSize: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "screencapture -x -R 1,2,30,40 /tmp/x.png" {
		t.Errorf("ran %q", got)
	}
}

func TestMacClipboardAndNotify(t *testing.T) {
	m := newMachine("darwin", "osascript", "pbcopy", "pbpaste")
	c := NewController(m.Env())
	ctx := context.Background()
	if err := c.ClipboardSet(ctx, "text"); err != nil {
		t.Fatal(err)
	}
	if m.calls[0].stdin != "text" || m.calls[0].argv[0] != "pbcopy" {
		t.Errorf("clipboard set ran %v", m.calls[0])
	}
	if err := c.Notify(ctx, "T", "B", "normal"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.lastCall()[2], `display notification "B" with title "T"`) {
		t.Errorf("notify script = %s", m.lastCall()[2])
	}
}

func TestWindowsListParsing(t *testing.T) {
	m := newMachine("windows", "powershell")
	m.outputs["powershell"] = "12345\t42\tcode\t0\t0\t1920\t1040\tfactor - Code\r\n"
	c := NewController(m.Env())
	wins, err := c.ListWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(wins) != 1 || wins[0].ID != "12345" || wins[0].PID != 42 || wins[0].Title != "factor - Code" {
		t.Fatalf("windows = %+v", wins)
	}
}

func TestWindowsQuotingAndKeys(t *testing.T) {
	if got := psQuote("it's"); got != "'it''s'" {
		t.Errorf("psQuote = %s", got)
	}
	if got := sendKeysEscape("100% {done}"); got != "100{%} {{}done{}}" {
		t.Errorf("sendKeysEscape = %s", got)
	}

	m := newMachine("windows", "powershell")
	c := NewController(m.Env())
	if err := c.PressKey(context.Background(), "ctrl+s", 1); err != nil {
		t.Fatal(err)
	}
	script := m.lastCall()[len(m.lastCall())-1]
	if !strings.Contains(script, "SendWait('^s')") {
		t.Errorf("script = %s", script)
	}
	if err := c.PressKey(context.Background(), "meta+x", 1); err == nil {
		t.Error("unknown modifier accepted")
	}
}

func TestWindowsMoveResizeFillsMissingHalf(t *testing.T) {
	m := newMachine("windows", "powershell")
	m.outputs["powershell"] = "99\t1\tapp\t10\t20\t300\t400\tApp\r\n"
	c := NewController(m.Env())
	err := c.MoveResize(context.Background(), Window{ID: "99"}, Geometry{W: 800, H: 600, HasSize: true})
	if err != nil {
		t.Fatal(err)
	}
	script := m.lastCall()[len(m.lastCall())-1]
	if !strings.Contains(script, "MoveWindow(") || !strings.Contains(script, ", 10, 20, 800, 600, $true)") {
		t.Errorf("script = %s", script)
	}
}

func TestWindowsPowerShellMissing(t *testing.T) {
	m := newMachine("windows")
	c := NewController(m.Env())
	if _, err := c.ListWindows(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "powershell") {
		t.Fatalf("error = %v", err)
	}
}

// TestLiveDesktop exercises the real machine when a graphical session with the
// helper tools is available; everywhere else (CI, headless boxes) it skips.
func TestLiveDesktop(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if underWine() {
		t.Skip("wine has no desktop to drive; this test needs real Windows")
	}
	env := DefaultEnv()
	if !HasDisplay(env) {
		t.Skip("no graphical session")
	}
	ctl := NewController(env)
	if len(MissingHelpers(env, ctl)) > 0 {
		t.Skipf("desktop helpers missing: %v", MissingHelpers(env, ctl))
	}
	ctx := context.Background()

	if _, err := ctl.ListWindows(ctx); err != nil {
		t.Errorf("ListWindows: %v", err)
	}
	if w, h, err := ctl.ScreenSize(ctx); err != nil || w == 0 || h == 0 {
		t.Errorf("ScreenSize = %d, %d, %v", w, h, err)
	}
	const marker = "factor-desktop-test"
	if err := ctl.ClipboardSet(ctx, marker); err != nil {
		t.Fatalf("ClipboardSet: %v", err)
	}
	got, err := ctl.ClipboardGet(ctx)
	if err != nil || strings.TrimSpace(got) != marker {
		t.Errorf("clipboard round-trip = %q, %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := ctl.Screenshot(ctx, path, Shot{Mode: "screen"}); err != nil {
		t.Errorf("Screenshot: %v", err)
	} else if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Errorf("screenshot file = %v, %v", info, err)
	}
}

// TestExecRunnerSeparatesStreams guards the property clipboard reads depend
// on: stderr must never end up mixed into the returned output.
func TestExecRunnerSeparatesStreams(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell")
	}
	run := execRunner(defaultRunnerTimeout)
	out, err := run(context.Background(), "", "sh", "-c", "echo out; echo err 1>&2")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(out) != "out" {
		t.Errorf("output = %q; stderr leaked into it", out)
	}

	out, err = run(context.Background(), "payload", "sh", "-c", "cat")
	if err != nil || strings.TrimSpace(out) != "payload" {
		t.Errorf("stdin round-trip = %q, %v", out, err)
	}

	if _, err := run(context.Background(), "", "sh", "-c", "echo boom 1>&2; exit 3"); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Errorf("failure error = %v; want the stderr detail", err)
	}
	if _, err := run(context.Background(), "", "factor-no-such-binary"); err == nil ||
		!strings.Contains(err.Error(), "not installed") {
		t.Errorf("missing binary error = %v", err)
	}
}

// TestExecRunnerIgnoresADetachedChild guards the clipboard: xclip, xsel and
// wl-copy all fork and stay resident to own the selection, so a runner that
// waited for the inherited output stream to close would turn every
// successful clipboard write into a two-second WaitDelay failure.
func TestExecRunnerIgnoresADetachedChild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell")
	}
	run := execRunner(defaultRunnerTimeout)
	started := time.Now()
	out, err := run(context.Background(), "", "sh", "-c", "sleep 5 & echo done")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(out) != "done" {
		t.Errorf("output = %q", out)
	}
	if waited := time.Since(started); waited > 1500*time.Millisecond {
		t.Errorf("waited %v on a child that had already detached", waited)
	}
}
