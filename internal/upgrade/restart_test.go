package upgrade

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestRestarterRequest(t *testing.T) {
	var nilRestarter *Restarter
	if nilRestarter.Request("no one is listening") {
		t.Error("a nil Restarter claimed it would restart")
	}

	r := &Restarter{}
	if r.Request("nothing registered yet") {
		t.Error("an unset Restarter claimed it would restart")
	}

	var got []string
	r.Set(func(reason string) { got = append(got, reason) })
	if !r.Request("installed factor v0.4.0") {
		t.Error("Request returned false with a restart registered")
	}
	if !reflect.DeepEqual(got, []string{"installed factor v0.4.0"}) {
		t.Errorf("restart reasons = %q", got)
	}
}

// startFrom stands in for the path this process was started from.
func startFrom(t *testing.T, path string, err error) {
	t.Helper()
	prevPath, prevErr := startPath, startPathErr
	startPath, startPathErr = path, err
	t.Cleanup(func() { startPath, startPathErr = prevPath, prevErr })
}

func TestSelfPathIsWhereTheProcessStarted(t *testing.T) {
	// Not os.Executable: by restart time an upgrade has renamed that inode
	// aside, and /proc/self/exe follows the inode rather than the name.
	startFrom(t, "/usr/local/bin/factor", nil)
	got, err := selfPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/local/bin/factor" {
		t.Errorf("selfPath() = %q", got)
	}

	startFrom(t, "", errors.New("no /proc"))
	if _, err := selfPath(); err == nil {
		t.Error("selfPath ignored an unreadable executable path")
	}
}

func TestRelaunchExecsTheBinaryOnDisk(t *testing.T) {
	exe := stageBinary(t, "the new build")
	startFrom(t, exe, nil)

	var gotPath string
	var gotArgv []string
	prev := execSelf
	execSelf = func(path string, argv, env []string) error {
		gotPath, gotArgv = path, argv
		if len(env) == 0 {
			t.Error("relaunched with an empty environment")
		}
		return nil
	}
	t.Cleanup(func() { execSelf = prev })

	if err := Relaunch(); err != nil {
		t.Fatal(err)
	}
	if gotPath != exe {
		t.Errorf("exec'd %q, want %q", gotPath, exe)
	}
	if !reflect.DeepEqual(gotArgv, os.Args) {
		t.Errorf("exec'd argv %q, want %q", gotArgv, os.Args)
	}
}

func TestToolRestartsAfterInstalling(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("the new build")}.start(t)
	stageBinary(t, "the old build")

	restarter := &Restarter{}
	var reasons []string
	restarter.Set(func(reason string) { reasons = append(reasons, reason) })

	res := (&Tool{Current: "v0.3.0", Restart: restarter}).Execute(context.Background(),
		map[string]any{"action": "install"})
	if res.IsError {
		t.Fatalf("install failed: %+v", res)
	}
	if !strings.Contains(res.ForLLM, "Restarting") {
		t.Errorf("result does not tell the agent it is restarting: %q", res.ForLLM)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "v0.4.0") {
		t.Errorf("restart reasons = %q", reasons)
	}
}

func TestToolWithoutARestarterSaysSo(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("the new build")}.start(t)
	stageBinary(t, "the old build")

	// A CLI session has nothing to reload, and must not promise otherwise.
	res := (&Tool{Current: "v0.3.0"}).Execute(context.Background(), map[string]any{"action": "install"})
	if res.IsError || !strings.Contains(res.ForLLM, "next start") {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(res.ForLLM, "Restarting") {
		t.Errorf("promised a restart with nothing to restart: %q", res.ForLLM)
	}
}
