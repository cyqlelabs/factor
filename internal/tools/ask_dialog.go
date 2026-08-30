package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// AskEnv is the seam between the dialog asker and the machine. Tests script
// it; DefaultAskEnv wires it to the real one.
type AskEnv struct {
	Run     func(ctx context.Context, argv ...string) (stdout string, code int, err error)
	Has     func(bin string) bool
	Display func() bool
	GOOS    string
}

// DefaultAskEnv reaches the machine this process runs on.
func DefaultAskEnv() AskEnv {
	return AskEnv{
		Run: askRunner,
		Has: func(bin string) bool { _, err := exec.LookPath(bin); return err == nil },
		Display: func() bool {
			switch runtime.GOOS {
			case "darwin", "windows":
				return true
			default:
				return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
			}
		},
		GOOS: runtime.GOOS,
	}
}

// askRunner runs one dialog program. Toolkit warnings on its stderr are
// neither an answer nor a failure and are dropped — all but the one line that
// says the window never opened, which every toolkit writes there and then
// exits with the very code it uses for "the user said no". Read from the exit
// code alone, a dead display becomes the agent telling a user who saw nothing
// that they dismissed the question.
func askRunner(ctx context.Context, argv ...string) (string, int, error) {
	if len(argv) == 0 {
		return "", -1, errors.New("no command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.String(), -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if neverOpened(errOut.String()) {
			return "", exitErr.ExitCode(), fmt.Errorf("%w: %s could not open the screen", ErrAskUnavailable, argv[0])
		}
		return out.String(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return out.String(), -1, err
	}
	return out.String(), 0, nil
}

// neverOpened spots the stderr line that means the dialog never reached a
// screen: the display named in the environment is not there any more.
func neverOpened(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "open display") || strings.Contains(s, "unable to init server")
}

// DialogAsker puts the question on the machine's own screen, in whatever
// dialog program the desktop already has. It is what the daemon uses: nobody
// is watching a terminal there, so the question has to come to the user.
type DialogAsker struct{ env AskEnv }

func NewDialogAsker(env AskEnv) *DialogAsker { return &DialogAsker{env: env} }

func (d *DialogAsker) Ask(ctx context.Context, q Question) (Answer, error) {
	if d.env.Run == nil {
		return Answer{}, fmt.Errorf("%w: no way to run a dialog", ErrAskUnavailable)
	}
	if d.env.Display != nil && !d.env.Display() {
		return Answer{}, fmt.Errorf("%w: this machine has no screen to show it on", ErrAskUnavailable)
	}
	switch d.env.GOOS {
	case "darwin":
		return d.run(ctx, q, []string{"osascript", "-e", appleScript(q)}, outcome{})
	case "windows":
		shell := d.first("pwsh", "powershell")
		if shell == "" {
			return Answer{}, fmt.Errorf("%w: no PowerShell on this machine", ErrAskUnavailable)
		}
		return d.run(ctx, q, []string{shell, "-NoProfile", "-Command", powerShellScript(q)}, outcome{})
	}
	switch d.first("zenity", "kdialog", "yad") {
	case "zenity":
		return d.run(ctx, q, zenityArgs(q), outcome{timedOut: 5, broken: 255})
	case "kdialog":
		return d.run(ctx, q, kdialogArgs(q), outcome{broken: 255})
	case "yad":
		return d.run(ctx, q, yadArgs(q), outcome{timedOut: 70, broken: 255, strip: "|"})
	}
	return Answer{}, fmt.Errorf("%w: install zenity, kdialog or yad to ask on screen", ErrAskUnavailable)
}

func (d *DialogAsker) first(bins ...string) string {
	if d.env.Has == nil {
		return ""
	}
	for _, b := range bins {
		if d.env.Has(b) {
			return b
		}
	}
	return ""
}

// outcome is how one dialog program spells its exit codes, and what it adds
// to the answer it prints.
type outcome struct {
	timedOut int    // gave up on its own; 0 when it cannot
	broken   int    // the program failed, which is not the user saying no
	strip    string // row separator the program appends (yad's "|")
}

// run executes one dialog and reads the result from its exit code: zero is an
// answer, the timeout code is silence, the broken code is a bug worth
// reporting, and anything else is the user closing the window.
//
// The order matters. zenity prints the row under the cursor even when it
// timed out with nobody there, so a timeout read as an answer would put words
// in the user's mouth.
func (d *DialogAsker) run(ctx context.Context, q Question, argv []string, codes outcome) (Answer, error) {
	out, code, err := d.env.Run(ctx, argv...)
	if err != nil {
		return Answer{}, err
	}
	switch {
	case codes.timedOut != 0 && code == codes.timedOut:
		return Answer{}, context.DeadlineExceeded
	case codes.broken != 0 && code == codes.broken:
		return Answer{}, fmt.Errorf("%s failed (exit %d)", argv[0], code)
	case code != 0:
		return Answer{Dismissed: true}, nil
	}
	text := strings.TrimSpace(out)
	if codes.strip != "" {
		text = strings.TrimSpace(strings.TrimSuffix(text, codes.strip))
	}
	if text == "" {
		return Answer{Dismissed: true}, nil
	}
	return Answer{Text: MatchOption(text, q.Options)}, nil
}

func zenityArgs(q Question) []string {
	args := []string{"zenity", "--title=Factor", "--timeout=" + strconv.Itoa(timeoutSecs(q))}
	if len(q.Options) == 0 {
		return append(args, "--entry", "--text="+q.Prompt)
	}
	args = append(args, "--list", "--text="+q.Prompt, "--column=Options", "--hide-header")
	return append(args, q.Options...)
}

// kdialogArgs numbers the options: --menu answers with the tag, and a number
// is the one tag that survives a label with spaces in it.
func kdialogArgs(q Question) []string {
	args := []string{"kdialog", "--title", "Factor"}
	if len(q.Options) == 0 {
		return append(args, "--inputbox", q.Prompt)
	}
	args = append(args, "--menu", q.Prompt)
	for i, o := range q.Options {
		args = append(args, strconv.Itoa(i+1), o)
	}
	return args
}

func yadArgs(q Question) []string {
	args := []string{"yad", "--title=Factor", "--center", "--timeout=" + strconv.Itoa(timeoutSecs(q))}
	if len(q.Options) == 0 {
		return append(args, "--entry", "--text="+q.Prompt)
	}
	args = append(args, "--list", "--text="+q.Prompt, "--column=Options", "--no-headers")
	return append(args, q.Options...)
}

// appleScript asks through System Events, which owns a window server session
// even when Factor itself has no UI. Giving up after the timeout leaves no
// dialog stranded on the screen.
func appleScript(q Question) string {
	var b strings.Builder
	b.WriteString("tell application \"System Events\"\n")
	if len(q.Options) == 0 {
		fmt.Fprintf(&b, "set r to display dialog %s default answer \"\" with title \"Factor\" giving up after %d\n",
			appleQuote(q.Prompt), timeoutSecs(q))
		b.WriteString("if gave up of r then return \"\"\n")
		b.WriteString("return text returned of r\n")
		b.WriteString("end tell")
		return b.String()
	}
	quoted := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		quoted = append(quoted, appleQuote(o))
	}
	fmt.Fprintf(&b, "set c to choose from list {%s} with title \"Factor\" with prompt %s\n",
		strings.Join(quoted, ", "), appleQuote(q.Prompt))
	b.WriteString("end tell\n")
	b.WriteString("if c is false then return \"\"\n")
	b.WriteString("return item 1 of c")
	return b.String()
}

// powerShellScript uses the input box every Windows install already has,
// listing the options in the prompt: one code path that cannot be missing.
func powerShellScript(q Question) string {
	prompt := q.Prompt
	if len(q.Options) > 0 {
		var b strings.Builder
		b.WriteString(prompt)
		for i, o := range q.Options {
			fmt.Fprintf(&b, "\n%d) %s", i+1, o)
		}
		b.WriteString("\n\nType a number, or your own answer.")
		prompt = b.String()
	}
	return "Add-Type -AssemblyName Microsoft.VisualBasic\n" +
		"Write-Output ([Microsoft.VisualBasic.Interaction]::InputBox(" +
		psQuote(prompt) + ", 'Factor', ''))"
}

func timeoutSecs(q Question) int {
	secs := int(q.Timeout.Seconds())
	if secs <= 0 {
		secs = int(defaultAskTimeout.Seconds())
	}
	return secs
}

func appleQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
