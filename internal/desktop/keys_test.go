package desktop

import (
	"context"
	"strings"
	"testing"
)

// The shortcuts that act on the session are refused before anything is sent,
// whatever they are spelled as, and a forced press goes through.
func TestPressKeyRefusesSessionShortcuts(t *testing.T) {
	for _, keys := range []string{
		"alt+Tab", "Alt+F4", "super", "Super_L", "cmd+tab", "ctrl+alt+Delete", "ctrl+alt+del",
		"control+alt+backspace", "ctrl+alt+F3", "win+l", "tab+alt", "ctrl+a alt+f4",
	} {
		t.Run(keys, func(t *testing.T) {
			m := newMachine("linux", "xdotool")
			byName, _ := newTools(t, m)
			res := run(t, byName["press_key"], map[string]any{"keys": keys})
			if !res.IsError || !strings.Contains(res.ForLLM, "Nothing was sent") {
				t.Fatalf("%q was not refused: %s", keys, res.ForLLM)
			}
			if len(m.calls) != 0 {
				t.Fatalf("a refused shortcut still ran %v", m.calls)
			}
		})
	}
}

func TestPressKeySendsOrdinaryAndForcedKeys(t *testing.T) {
	for _, keys := range []string{"ctrl+s", "Return", "ctrl+q", "alt+f", "shift+tab", "super+e"} {
		m := newMachine("linux", "xdotool")
		byName, _ := newTools(t, m)
		if res := run(t, byName["press_key"], map[string]any{"keys": keys}); res.IsError {
			t.Errorf("%q refused: %s", keys, res.ForLLM)
		}
	}
	m := newMachine("linux", "xdotool")
	byName, _ := newTools(t, m)
	res := run(t, byName["press_key"], map[string]any{"keys": "alt+tab", "force": true})
	if res.IsError || !m.ranWith("alt+tab") {
		t.Fatalf("forced press did not go through: %s %v", res.ForLLM, m.calls)
	}
}

func TestCanonicalCombo(t *testing.T) {
	for in, want := range map[string]string{
		"Tab+Alt":          "alt+tab",
		"Super_R":          "super",
		"shift+ctrl+T":     "ctrl+shift+t",
		"cmd+Option+Esc":   "alt+super+escape",
		"ctrl++":           "ctrl",
		"Control_L+Del":    "ctrl+delete",
		"ISO_Left_Tab+alt": "alt+tab",
	} {
		if got := canonicalCombo(in); got != want {
			t.Errorf("canonicalCombo(%q) = %q, want %q", in, got, want)
		}
	}
}

// A newline typed as a keystroke is Return: a chat box sends after the first
// line and the rest is typed into whatever comes next.
func TestTypeTextRefusesControlCharacters(t *testing.T) {
	for text, name := range map[string]string{
		"first line\nsecond": "a newline",
		"name\tpassword":     "a tab",
		"a\rb":               "a carriage return",
		"bell\a":             "U+0007",
	} {
		m := newMachine("linux", "xdotool")
		byName, _ := newTools(t, m)
		res := run(t, byName["type_text"], map[string]any{"text": text})
		if !res.IsError || !strings.Contains(res.ForLLM, name) {
			t.Errorf("%q: %s", text, res.ForLLM)
		}
		if len(m.calls) != 0 {
			t.Errorf("%q still ran %v", text, m.calls)
		}
	}
}

// What is typed or copied never reaches a command line on Windows: it rides
// stdin, and the script reads it back as UTF-8.
func TestWindowsTextRidesStdin(t *testing.T) {
	ctx := context.Background()
	secret := "pässwörd%{+}"
	for name, call := range map[string]func(Controller) error{
		"type":      func(c Controller) error { return c.TypeText(ctx, secret, 0) },
		"clipboard": func(c Controller) error { return c.ClipboardSet(ctx, secret) },
	} {
		m, c := winCtl(t, "powershell")
		if err := call(c); err != nil {
			t.Fatal(err)
		}
		argv := m.lastCall()
		if strings.Contains(strings.Join(argv, " "), "pässwörd") {
			t.Errorf("%s: the text is in the command line: %v", name, argv)
		}
		if m.lastStdin() != secret {
			t.Errorf("%s: stdin = %q", name, m.lastStdin())
		}
		if !strings.Contains(argv[len(argv)-1], "UTF8.GetString") {
			t.Errorf("%s: the script does not decode stdin as UTF-8: %s", name, argv[len(argv)-1])
		}
	}
}

// osascript reads its script from stdin, which keeps typed text and
// notification bodies out of the process list.
func TestMacScriptsRideStdin(t *testing.T) {
	m, c := macCtl(t, "osascript")
	if err := c.TypeText(context.Background(), "hunter2", 0); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "osascript -" {
		t.Errorf("ran %q", got)
	}
	if !strings.Contains(m.lastStdin(), `keystroke "hunter2"`) {
		t.Errorf("script = %q", m.lastStdin())
	}
}
