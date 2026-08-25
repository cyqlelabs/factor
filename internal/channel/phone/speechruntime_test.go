package phone

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// windowsMachine points the package at Windows and hands back a recorder for
// everything it shells out to.
func windowsMachine(t *testing.T) *[][]string {
	t.Helper()
	prevOS, prevCmd := goos, runCmd
	t.Cleanup(func() { goos, runCmd = prevOS, prevCmd })
	goos = "windows"
	var ran [][]string
	runCmd = func(_ context.Context, argv []string) (string, error) {
		ran = append(ran, argv)
		return "", nil
	}
	return &ran
}

// servedRedist stands in for Microsoft's permalink so no test reaches the
// network.
func servedRedist(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("MZ"))
	}))
	prev := vcRedistURL
	t.Cleanup(func() { vcRedistURL = prev; server.Close() })
	vcRedistURL = server.URL
}

// The bug: a fresh Windows has no msvcp140.dll, so pip installs every engine
// and the first import of onnxruntime fails on a DLL the machine never had.
// Handing the user a traceback is the failure — the installer installs it.
func TestSpeechRuntimeInstallsTheMissingWindowsCppRuntime(t *testing.T) {
	ran := windowsMachine(t)
	servedRedist(t)

	var probes int
	runCmd = func(_ context.Context, argv []string) (string, error) {
		*ran = append(*ran, argv)
		if len(argv) > 1 && argv[1] == "-c" {
			probes++
			if probes == 1 {
				return "ImportError: DLL load failed while importing onnxruntime_pybind11_state: " +
					"The specified module could not be found.", errors.New("exit status 1")
			}
			return "", nil
		}
		return "", nil
	}

	var steps []string
	err := ensureSpeechRuntime(context.Background(), `C:\python.exe`,
		func(format string, args ...any) { steps = append(steps, fmt.Sprintf(format, args...)) })
	if err != nil {
		t.Fatalf("ensureSpeechRuntime: %v", err)
	}

	var installed bool
	for _, argv := range *ran {
		if strings.Contains(argv[0], "vc_redist") {
			installed = true
			for _, want := range []string{"/install", "/quiet", "/norestart"} {
				if !contains(argv, want) {
					t.Errorf("the runtime installer ran without %s: %v", want, argv)
				}
			}
		}
	}
	if !installed {
		t.Fatalf("the missing C++ runtime was never installed: %v", *ran)
	}
	if probes != 2 {
		t.Errorf("probed %d times, want a re-probe proving the fix took", probes)
	}
	if len(steps) == 0 {
		t.Error("the user was told nothing while a download ran")
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), "factor-vc_redist.x64.exe")); err == nil {
		t.Error("the downloaded installer was left behind")
	}
}

// A runtime already current exits 1638 and one that wants a reboot exits 3010.
// Both put the DLLs in place, so neither is a failed install.
func TestSpeechRuntimeAcceptsTheInstallersNonZeroSuccesses(t *testing.T) {
	for _, code := range []int{vcRedistAlreadyCurrent, vcRedistRebootRequired} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			ran := windowsMachine(t)
			servedRedist(t)

			var probes int
			runCmd = func(_ context.Context, argv []string) (string, error) {
				*ran = append(*ran, argv)
				if len(argv) > 1 && argv[1] == "-c" {
					probes++
					if probes == 1 {
						return "DLL load failed", errors.New("exit status 1")
					}
					return "", nil
				}
				return "", exitErr(code)
			}
			if err := ensureSpeechRuntime(context.Background(), `C:\python.exe`, nil); err != nil {
				t.Fatalf("exit %d should be a success: %v", code, err)
			}
		})
	}
}

// An import that fails for any other reason is a different problem, and
// installing a C++ runtime over it would hide it.
func TestSpeechRuntimeLeavesOtherImportFailuresAlone(t *testing.T) {
	ran := windowsMachine(t)
	runCmd = func(_ context.Context, argv []string) (string, error) {
		*ran = append(*ran, argv)
		return "ModuleNotFoundError: No module named 'onnxruntime'", errors.New("exit status 1")
	}
	err := ensureSpeechRuntime(context.Background(), `C:\python.exe`, nil)
	if err == nil {
		t.Fatal("a venv that cannot import its engines should not be reported ready")
	}
	if !strings.Contains(err.Error(), "ModuleNotFoundError") {
		t.Errorf("error = %q, want it to carry what Python actually said", err)
	}
	for _, argv := range *ran {
		if strings.Contains(argv[0], "vc_redist") {
			t.Errorf("a missing module triggered a runtime install: %v", argv)
		}
	}
}

// Elevation is the one thing the installer cannot get for itself, so the
// failure has to say so instead of naming a download.
func TestSpeechRuntimeNamesElevationWhenTheInstallerFails(t *testing.T) {
	ran := windowsMachine(t)
	servedRedist(t)
	runCmd = func(_ context.Context, argv []string) (string, error) {
		*ran = append(*ran, argv)
		if len(argv) > 1 && argv[1] == "-c" {
			return "DLL load failed", errors.New("exit status 1")
		}
		return "", exitErr(5)
	}
	err := ensureSpeechRuntime(context.Background(), `C:\python.exe`, nil)
	if err == nil {
		t.Fatal("a refused install should fail")
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("error = %q, want the reason it could not install", err)
	}
}

// Only Windows ships an incomplete C++ runtime; nowhere else pays for a probe.
func TestSpeechRuntimeIsAWindowsProblemOnly(t *testing.T) {
	ran := windowsMachine(t)
	goos = "linux"
	if err := ensureSpeechRuntime(context.Background(), "/usr/bin/python3", nil); err != nil {
		t.Fatalf("ensureSpeechRuntime: %v", err)
	}
	if len(*ran) != 0 {
		t.Errorf("ran %v off Windows, want nothing", *ran)
	}
}

func contains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// exitErr is an installer that answered with a status and nothing else, which
// is how the real one reports every outcome it has.
type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }
