package phone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Every speech engine Factor installs — Piper, faster-whisper, onnx-asr,
// sherpa-onnx — is a model on top of the same native stack, and on Windows
// that stack needs a C++ runtime the machine does not ship with. Python's own
// installer carries vcruntime140.dll because CPython is written in C; nothing
// carries msvcp140.dll, because CPython is not written in C++. So a fresh
// Windows — the one a user most likely runs `factor init` on — pip-installs
// every engine successfully and then cannot import one of them:
//
//	ImportError: DLL load failed while importing onnxruntime_pybind11_state:
//	The specified module could not be found.
//
// The venv is not broken and reinstalling it changes nothing, which is why
// this is checked as its own step rather than left to be read out of a
// traceback: the missing piece is a Microsoft redistributable, and installing
// what is missing is the installer's job.

// goos is a seam so the Windows path can be tested from anywhere.
var goos = runtime.GOOS

// vcRedistURL is Microsoft's permalink to the current x64 VC++ 2015–2022
// redistributable. It is a moving target by design — the engines want 14.40 or
// newer and an older one on the machine fails the same way a missing one does —
// so there is no checksum to pin it against, only the host it comes from.
var vcRedistURL = "https://aka.ms/vs/17/release/vc_redist.x64.exe"

const (
	// speechRuntimeProbe is the smallest thing that proves the native stack
	// will load. Importing onnxruntime pulls the whole C++ runtime in with it.
	speechRuntimeProbe = "import onnxruntime"

	// dllLoadFailure is what Windows says when a DLL's own dependency is
	// missing, and the one failure this step can fix. Anything else importing
	// onnxruntime raises is a different problem and is reported as itself.
	dllLoadFailure = "DLL load failed"

	// vcRedistTimeout bounds the download and the install together. It is a
	// 25MB file and a few seconds of work; the slack is for the connection.
	vcRedistTimeout = 10 * time.Minute

	// The installer's two non-zero successes: the machine already has this
	// version or newer, and the files are in place but a reboot is owed for
	// the ones that were in use.
	vcRedistAlreadyCurrent = 1638
	vcRedistRebootRequired = 3010

	// vcRedistElevationHint explains the one failure Factor cannot install its
	// way out of. The redistributable writes to System32, so it needs
	// administrator rights: with a desktop to prompt on, Windows asks for them
	// and the user says yes, but nothing can ask on behalf of a service.
	vcRedistElevationHint = "the Microsoft Visual C++ runtime installs into System32, which needs administrator rights — run `factor init` from a terminal opened as administrator"
)

// ensureSpeechRuntime makes the engines importable, installing the C++ runtime
// they load through when Windows has none. It is a no-op everywhere else: only
// Windows ships an incomplete one.
func ensureSpeechRuntime(ctx context.Context, python string, progress Progress) error {
	if goos != "windows" {
		return nil
	}
	out, err := runCmd(ctx, []string{python, "-c", speechRuntimeProbe})
	if err == nil {
		return nil
	}
	if !strings.Contains(out, dllLoadFailure) {
		return fmt.Errorf("the speech engines are installed but will not load: %v\n%s", err, lastLines(out, 8))
	}

	progress.emit("installing the Microsoft Visual C++ runtime the speech engines load through…")
	if err := installVCRedist(ctx); err != nil {
		return fmt.Errorf("%w\n%s", err, vcRedistElevationHint)
	}
	if out, err := runCmd(ctx, []string{python, "-c", speechRuntimeProbe}); err != nil {
		return fmt.Errorf("the Visual C++ runtime installed and onnxruntime still will not load — if the installer asked for a reboot, it is owed one: %v\n%s",
			err, lastLines(out, 8))
	}
	progress.emit("the Visual C++ runtime is in place")
	return nil
}

// installVCRedist downloads Microsoft's redistributable and runs it
// unattended. It asks Windows for elevation on its own, so a user sitting at
// the machine answers one dialog and a machine with nobody at it fails here
// with a reason rather than in a traceback twenty minutes later.
func installVCRedist(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, vcRedistTimeout)
	defer cancel()

	installer := filepath.Join(os.TempDir(), "factor-vc_redist.x64.exe")
	if err := fetchFile(ctx, vcRedistURL, installer); err != nil {
		return fmt.Errorf("could not download the Visual C++ runtime: %w", err)
	}
	defer func() { _ = os.Remove(installer) }()

	out, err := runCmd(ctx, []string{installer, "/install", "/quiet", "/norestart"})
	if err == nil {
		return nil
	}
	switch exitCode(err) {
	case vcRedistAlreadyCurrent, vcRedistRebootRequired:
		return nil
	}
	return fmt.Errorf("the Visual C++ runtime installer failed: %v\n%s", err, lastLines(out, 6))
}

// exitCode reads the installer's answer, which is the whole of what it says:
// it prints nothing and reports through its status. Matching the method rather
// than *exec.ExitError is what keeps the runCmd seam testable without spawning
// a process to fail on purpose.
func exitCode(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

func fetchFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(dest)
		return err
	}
	return f.Close()
}
