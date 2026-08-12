package wizard

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
)

// plainUI drives the non-terminal path: the prompts a piped stdin sees.
func plainUI(answers ...string) (*UI, *bytes.Buffer) {
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(answers, "\n") + "\n")
	return NewPlain(in, &out), &out
}

// colorUI is a UI that believes it is on a colour terminal without one being
// present, so the styled output paths are exercised off a TTY.
func colorUI(answers ...string) (*UI, *bytes.Buffer) {
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(answers, "\n") + "\n")
	return &UI{reader: bufio.NewReader(in), out: &out, color: true}, &out
}

func TestPlainUIWritesNoEscapes(t *testing.T) {
	u, out := plainUI()
	u.Banner("v9")
	u.Step(2, 5, "Provider")
	u.Info("info")
	u.Note("note")
	u.Success("success")
	u.Warn("warn")
	u.Fail("fail")
	u.Summary("Setup", [][2]string{{"provider", "ollama"}, {"a", "b"}})

	got := out.String()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("plain UI emitted ANSI escapes:\n%q", got)
	}
	for _, want := range []string{"v9", "[2/5] Provider", "info", "note", "success", "warn", "fail", "provider  ollama"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// Summary pads the key column to the widest key
	if !strings.Contains(got, "a         b") {
		t.Errorf("summary is not aligned:\n%s", got)
	}
}

func TestColorUIStyles(t *testing.T) {
	u, out := colorUI()
	u.Banner("v9")
	u.Fail("nope")
	if !strings.Contains(out.String(), ansiRed) || !strings.Contains(out.String(), ansiCyan) {
		t.Errorf("styled output has no colour:\n%q", out.String())
	}
}

func TestInputDefaultsAndAnswers(t *testing.T) {
	u, out := plainUI("", "typed")
	got, err := u.Input("model?", "gpt-5")
	if err != nil || got != "gpt-5" {
		t.Fatalf("blank answer = %q, %v; want the default", got, err)
	}
	if !strings.Contains(out.String(), "[gpt-5]") {
		t.Errorf("the default was not shown: %q", out.String())
	}
	if got, err := u.Input("model?", "gpt-5"); err != nil || got != "typed" {
		t.Fatalf("typed answer = %q, %v", got, err)
	}

	// EOF is a cancelled setup, not an empty answer
	u, _ = plainUI()
	_, _ = u.Input("a?", "")
	if _, err := u.Input("b?", ""); !errors.Is(err, ErrAborted) {
		t.Errorf("EOF = %v, want ErrAborted", err)
	}
}

func TestSecretKeepsExistingOnBlank(t *testing.T) {
	u, out := plainUI("", "sk-new")
	got, err := u.Secret("API key", "sk-old")
	if err != nil || got != "sk-old" {
		t.Fatalf("blank answer = %q, %v; want the existing key", got, err)
	}
	if strings.Contains(out.String(), "sk-old") {
		t.Errorf("the existing secret was echoed: %q", out.String())
	}
	if !strings.Contains(out.String(), "keep existing") {
		t.Errorf("no hint that a key is already set: %q", out.String())
	}
	if got, err := u.Secret("API key", "sk-old"); err != nil || got != "sk-new" {
		t.Fatalf("typed answer = %q, %v", got, err)
	}

	u, _ = plainUI()
	_, _ = u.Secret("API key", "")
	if _, err := u.Secret("API key", ""); !errors.Is(err, ErrAborted) {
		t.Errorf("EOF = %v, want ErrAborted", err)
	}
}

func TestConfirm(t *testing.T) {
	u, out := plainUI("maybe", "yes", "n", "")
	got, err := u.Confirm("install?", true)
	if err != nil || !got {
		t.Fatalf("answer after a retry = %v, %v", got, err)
	}
	if !strings.Contains(out.String(), "please answer y or n") {
		t.Errorf("garbage was accepted silently: %q", out.String())
	}
	if got, err := u.Confirm("install?", true); err != nil || got {
		t.Fatalf(`"n" = %v, %v`, got, err)
	}
	if got, err := u.Confirm("install?", true); err != nil || !got {
		t.Fatalf("blank = %v, %v; want the default", got, err)
	}
	if !strings.Contains(out.String(), "[Y/n]") {
		t.Errorf("the default is not visible in the prompt: %q", out.String())
	}

	u, out = plainUI("")
	if got, _ := u.Confirm("install?", false); got {
		t.Error("blank did not take the false default")
	}
	if !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("prompt = %q", out.String())
	}

	u, _ = plainUI()
	_, _ = u.Confirm("a?", false)
	if _, err := u.Confirm("b?", false); !errors.Is(err, ErrAborted) {
		t.Errorf("EOF = %v, want ErrAborted", err)
	}
}

func TestSelectNumbered(t *testing.T) {
	opts := []Option{{Label: "one", Hint: "first"}, {Label: "two"}, {Label: "three"}}

	u, out := plainUI("")
	if idx, err := u.Select("pick", opts, 1); err != nil || idx != 1 {
		t.Fatalf("blank = %d, %v; want the default", idx, err)
	}
	if !strings.Contains(out.String(), "choose 1-3 [2]") {
		t.Errorf("prompt = %q", out.String())
	}
	if !strings.Contains(out.String(), "1) one  first") {
		t.Errorf("hints are not shown: %q", out.String())
	}

	u, out = plainUI("nine", "0", "3")
	if idx, err := u.Select("pick", opts, 0); err != nil || idx != 2 {
		t.Fatalf("answer after retries = %d, %v", idx, err)
	}
	if n := strings.Count(out.String(), "enter a number between 1 and 3"); n != 2 {
		t.Errorf("out-of-range answers rejected %d times: %q", n, out.String())
	}

	// an out-of-range default is clamped rather than returned
	u, _ = plainUI("")
	if idx, err := u.Select("pick", opts, 99); err != nil || idx != 0 {
		t.Fatalf("clamped default = %d, %v", idx, err)
	}

	u, _ = plainUI()
	if _, err := u.Select("pick", nil, 0); err == nil {
		t.Error("a menu with no options was accepted")
	}
	u, _ = plainUI()
	_, _ = u.Select("pick", opts, 0)
	if _, err := u.Select("pick", opts, 0); !errors.Is(err, ErrAborted) {
		t.Errorf("EOF = %v, want ErrAborted", err)
	}
}

func TestMultiSelectNumbered(t *testing.T) {
	opts := []Option{{Label: "telegram", Hint: "chat"}, {Label: "cli"}, {Label: "http"}}

	// blank keeps the presets, and the presets are offered as the default
	u, out := plainUI("")
	got, err := u.MultiSelect("channels", opts, []bool{true, false, true})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0] || got[1] || !got[2] {
		t.Errorf("blank changed the selection: %v", got)
	}
	if !strings.Contains(out.String(), "[1,3]") {
		t.Errorf("presets are not the default: %q", out.String())
	}
	if !strings.Contains(out.String(), "[x] 1) telegram") {
		t.Errorf("preset marks missing: %q", out.String())
	}

	// an explicit list replaces the presets, junk entries and all
	u, _ = plainUI("2, 3, 99, x")
	got, err = u.MultiSelect("channels", opts, []bool{true, false, false})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] || !got[1] || !got[2] {
		t.Errorf("selection = %v, want only 2 and 3", got)
	}

	// "none" clears everything
	u, _ = plainUI("none")
	if got, err = u.MultiSelect("channels", opts, []bool{true, true, true}); err != nil {
		t.Fatal(err)
	}
	for i, on := range got {
		if on {
			t.Errorf("%q survived \"none\"", opts[i].Label)
		}
	}

	// a mismatched selected slice is replaced rather than indexed out of range
	u, _ = plainUI("1")
	if got, err = u.MultiSelect("channels", opts, []bool{true}); err != nil || !got[0] || got[1] {
		t.Fatalf("selection = %v, %v", got, err)
	}

	u, _ = plainUI()
	_, _ = u.MultiSelect("channels", opts, nil)
	if _, err := u.MultiSelect("channels", opts, nil); !errors.Is(err, ErrAborted) {
		t.Errorf("EOF = %v, want ErrAborted", err)
	}
}

func TestTaskReportsOutcome(t *testing.T) {
	u, out := plainUI()
	if err := u.Task("installing", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "installing… ok") {
		t.Errorf("output = %q", out.String())
	}

	boom := errors.New("boom")
	if err := u.Task("installing", func() error { return boom }); !errors.Is(err, boom) {
		t.Errorf("Task swallowed the error: %v", err)
	}
	if !strings.Contains(out.String(), "failed: boom") {
		t.Errorf("output = %q", out.String())
	}
}

// On a colour terminal the same task runs behind a spinner, and still has to
// return fn's error untouched.
func TestTaskSpinnerPath(t *testing.T) {
	u, out := colorUI()
	if err := u.Task("probing", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "probing") || !strings.Contains(out.String(), "✓") {
		t.Errorf("output = %q", out.String())
	}

	boom := errors.New("no route to host")
	if err := u.Task("probing", func() error { return boom }); !errors.Is(err, boom) {
		t.Errorf("Task swallowed the error: %v", err)
	}
	if !strings.Contains(out.String(), "no route to host") {
		t.Errorf("output = %q", out.String())
	}
}

func TestProgressPrintsSubSteps(t *testing.T) {
	u, out := plainUI()
	report := u.Progress()
	report("trying %s", "pipx")
	report("trying %s", "pip")
	got := out.String()
	if !strings.Contains(got, "trying pipx") || !strings.Contains(got, "trying pip") {
		t.Errorf("output = %q", got)
	}
}

func TestNewPlainIsNotInteractive(t *testing.T) {
	u, _ := plainUI()
	if u.Interactive() {
		t.Error("a piped UI claims to be interactive")
	}
	// New over a non-terminal file agrees
	u2 := New(nil, &bytes.Buffer{})
	if u2.Interactive() {
		t.Error("a nil input claims to be interactive")
	}
}
