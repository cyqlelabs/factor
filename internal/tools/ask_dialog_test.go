package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeMachine scripts one dialog program: what it prints, what it exits with,
// and what argv it was handed.
type fakeMachine struct {
	goos    string
	bins    map[string]bool
	display bool

	out  string
	code int
	err  error

	argv []string
}

func (m *fakeMachine) env() AskEnv {
	return AskEnv{
		Run: func(_ context.Context, argv ...string) (string, int, error) {
			m.argv = argv
			return m.out, m.code, m.err
		},
		Has:     func(bin string) bool { return m.bins[bin] },
		Display: func() bool { return m.display },
		GOOS:    m.goos,
	}
}

func machineWith(goos string, bins ...string) *fakeMachine {
	m := &fakeMachine{goos: goos, bins: map[string]bool{}, display: true}
	for _, b := range bins {
		m.bins[b] = true
	}
	return m
}

func question(prompt string, options ...string) Question {
	return Question{Prompt: prompt, Options: options, Timeout: 90 * time.Second}
}

func TestDialogZenity(t *testing.T) {
	m := machineWith("linux", "zenity")
	m.out = "SQLite\n"
	answer, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which database?", "Postgres", "SQLite"))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Text != "SQLite" || answer.Dismissed {
		t.Errorf("answer = %+v", answer)
	}
	joined := strings.Join(m.argv, " ")
	for _, want := range []string{"zenity", "--list", "--text=Which database?", "--timeout=90", "Postgres", "SQLite"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %v is missing %q", m.argv, want)
		}
	}

	// An open question is an entry box, not a list.
	m.out = "anything"
	if _, err := NewDialogAsker(m.env()).Ask(context.Background(), question("What name?")); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(m.argv, " "); !strings.Contains(joined, "--entry") || strings.Contains(joined, "--list") {
		t.Errorf("open question argv = %v", m.argv)
	}
}

func TestDialogZenityCancelAndTimeout(t *testing.T) {
	m := machineWith("linux", "zenity")
	m.code = 1
	answer, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?"))
	if err != nil || !answer.Dismissed {
		t.Errorf("cancel: answer = %+v, err = %v", answer, err)
	}

	m.code = 5 // zenity's own timeout
	if _, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: err = %v, want a deadline", err)
	}

	// A dialog that answers with nothing answered nothing.
	m.code, m.out = 0, "   \n"
	answer, err = NewDialogAsker(m.env()).Ask(context.Background(), question("Which?"))
	if err != nil || !answer.Dismissed {
		t.Errorf("blank: answer = %+v, err = %v", answer, err)
	}
}

func TestDialogKdialogNumbersOptions(t *testing.T) {
	m := machineWith("linux", "kdialog")
	m.out = "2\n" // --menu answers with the tag
	answer, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which database?", "Postgres", "SQLite"))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Text != "SQLite" {
		t.Errorf("answer = %q, want the label behind tag 2", answer.Text)
	}
	joined := strings.Join(m.argv, " ")
	if !strings.Contains(joined, "--menu") || !strings.Contains(joined, "1 Postgres 2 SQLite") {
		t.Errorf("argv = %v", m.argv)
	}
	if _, err := NewDialogAsker(m.env()).Ask(context.Background(), question("What name?")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(m.argv, " "), "--inputbox") {
		t.Errorf("open question argv = %v", m.argv)
	}
}

func TestDialogYadStripsSeparator(t *testing.T) {
	m := machineWith("linux", "yad")
	m.out = "Postgres|\n" // yad terminates list rows with its separator
	answer, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?", "Postgres", "SQLite"))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Text != "Postgres" {
		t.Errorf("answer = %q", answer.Text)
	}
	if !strings.Contains(strings.Join(m.argv, " "), "--no-headers") {
		t.Errorf("argv = %v", m.argv)
	}
	m.code = 255 // a flag yad does not understand is not the user saying no
	if _, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?")); err == nil {
		t.Error("a broken dialog program should be an error, not a dismissal")
	}
	m.code = 70 // yad's timeout code
	if _, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline", err)
	}
}

func TestDialogMacOS(t *testing.T) {
	m := machineWith("darwin")
	m.out = "SQLite\n"
	if _, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?", "Postgres", "SQLite")); err != nil {
		t.Fatal(err)
	}
	script := m.argv[2]
	for _, want := range []string{"choose from list", `"Postgres", "SQLite"`, "System Events"} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}

	m.out = "Roxana"
	if _, err := NewDialogAsker(m.env()).Ask(context.Background(), question(`Say "hi"?`)); err != nil {
		t.Fatal(err)
	}
	script = m.argv[2]
	if !strings.Contains(script, "display dialog") || !strings.Contains(script, `\"hi\"`) {
		t.Errorf("quoting is wrong:\n%s", script)
	}
	if !strings.Contains(script, "giving up after 90") {
		t.Errorf("script does not give up:\n%s", script)
	}
}

func TestDialogWindows(t *testing.T) {
	m := machineWith("windows", "powershell")
	m.out = "2"
	answer, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which? It's a choice", "Postgres", "SQLite"))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Text != "SQLite" {
		t.Errorf("answer = %q, want the option the number stands for", answer.Text)
	}
	script := m.argv[3]
	for _, want := range []string{"InputBox", "1) Postgres", "2) SQLite", "It''s a choice"} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
	if m.argv[0] != "powershell" {
		t.Errorf("shell = %q", m.argv[0])
	}
}

func TestDialogUnavailable(t *testing.T) {
	tests := map[string]*fakeMachine{
		"no dialog program": machineWith("linux"),
		"no powershell":     machineWith("windows"),
	}
	for name, m := range tests {
		if _, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?")); !errors.Is(err, ErrAskUnavailable) {
			t.Errorf("%s: err = %v, want unavailable", name, err)
		}
	}

	headless := machineWith("linux", "zenity")
	headless.display = false
	if _, err := NewDialogAsker(headless.env()).Ask(context.Background(), question("Which?")); !errors.Is(err, ErrAskUnavailable) {
		t.Errorf("headless: err = %v, want unavailable", err)
	}
	if _, err := NewDialogAsker(AskEnv{GOOS: "linux"}).Ask(context.Background(), question("Which?")); !errors.Is(err, ErrAskUnavailable) {
		t.Errorf("no runner: err = %v, want unavailable", err)
	}
}

func TestDialogBrokenProgramIsNotADismissal(t *testing.T) {
	m := machineWith("linux", "zenity")
	m.code, m.out = 255, ""
	_, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?"))
	if err == nil || !strings.Contains(err.Error(), "zenity failed") {
		t.Errorf("err = %v, want the program's own failure", err)
	}
}

// zenity prints the row under the cursor even when nobody was there to pick
// it, so the timeout has to win over the text.
func TestDialogTimeoutBeatsPrintedRow(t *testing.T) {
	m := machineWith("linux", "zenity")
	m.out, m.code = "Postgres\n", 5
	answer, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?", "Postgres", "SQLite"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline", err)
	}
	if answer.Text != "" {
		t.Errorf("answer = %q, want nothing put in the user's mouth", answer.Text)
	}
}

func TestDialogYadKeepsPipesInsideAnswers(t *testing.T) {
	m := machineWith("linux", "yad")
	m.out = "a|b|\n" // only the trailing separator belongs to yad
	answer, err := NewDialogAsker(m.env()).Ask(context.Background(), question("What?"))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Text != "a|b" {
		t.Errorf("answer = %q, want the inner separator kept", answer.Text)
	}
	// Every other program's answers are left exactly as typed.
	z := machineWith("linux", "zenity")
	z.out = "a|b|"
	answer, err = NewDialogAsker(z.env()).Ask(context.Background(), question("What?"))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Text != "a|b|" {
		t.Errorf("answer = %q, want it untouched", answer.Text)
	}
}

func TestDialogRunFailure(t *testing.T) {
	m := machineWith("linux", "zenity")
	m.err = errors.New("cannot open display")
	if _, err := NewDialogAsker(m.env()).Ask(context.Background(), question("Which?")); err == nil {
		t.Error("a broken dialog program should surface its error")
	}
}

func TestAskRunnerReportsExitCode(t *testing.T) {
	out, code, err := askRunner(context.Background(), winArgv("echo hello; exit 3", "(echo hello)& exit /b 3")...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "hello" || code != 3 {
		t.Errorf("out = %q, code = %d", out, code)
	}
	if _, _, err := askRunner(context.Background()); err == nil {
		t.Error("an empty command should fail")
	}
	if _, _, err := askRunner(context.Background(), "factor-no-such-dialog-program"); err == nil {
		t.Error("a missing program should fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := askRunner(ctx, "sh", "-c", "sleep 5"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the context error", err)
	}
}

func TestDefaultAskEnv(t *testing.T) {
	env := DefaultAskEnv()
	if env.Run == nil || env.Has == nil || env.Display == nil || env.GOOS == "" {
		t.Fatal("DefaultAskEnv is not fully wired")
	}
	if env.Has("factor-no-such-dialog-program") {
		t.Error("Has should not find a program that does not exist")
	}
	env.Display() // platform-dependent, but it must not panic
}

func TestTimeoutSecsFallsBack(t *testing.T) {
	if got := timeoutSecs(Question{}); got != int(defaultAskTimeout.Seconds()) {
		t.Errorf("timeoutSecs = %d, want the default", got)
	}
}
