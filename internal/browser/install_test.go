//go:build !nobrowser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// noBrowserAnywhere removes the second half of discovery. PATH can be pointed
// at an empty directory, but the fixed locations — a Mac's /Applications, a
// machine that already has an engine provisioned — cannot be emptied, so the
// lookup itself is replaced.
func noBrowserAnywhere(t *testing.T) {
	t.Helper()
	fakeBrowserOnPath(t)
	old := wellKnownBrowsers
	wellKnownBrowsers = func() []string { return nil }
	t.Cleanup(func() { wellKnownBrowsers = old })
}

// releaseServer serves a GitHub release index plus the tarball it points at.
func releaseServer(t *testing.T, assetName string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/download") {
			w.Header().Set("Content-Length", "4")
			fmt.Fprint(w, "data")
			return
		}
		fmt.Fprintf(w, `{"tag_name":"9.9.9","assets":[{"name":%q,"browser_download_url":%q,"size":4}]}`,
			assetName, srv.URL+"/download")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func linuxAssetName(t *testing.T) string {
	t.Helper()
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		t.Skipf("no Helium build for %s", runtime.GOARCH)
	}
	return "helium-9.9.9-" + arch + "_linux.tar.xz"
}

// useServer points the release lookup at srv and restores the default after.
func useServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := releaseAPI
	releaseAPI = srv.URL + "/%s"
	t.Cleanup(func() { releaseAPI = old })
}

// stubExec replaces the two shell seams: tar unpacks by creating the
// directory layout the real archive would have, and --version answers.
func stubExec(t *testing.T, unpackTo func(dir string)) {
	t.Helper()
	oldLook, oldRun := lookPath, runCmd
	lookPath = func(string) (string, error) { return "/usr/bin/tar", nil }
	runCmd = func(_ context.Context, argv []string) (string, error) {
		if argv[0] == "tar" {
			unpackTo(argv[len(argv)-1])
			return "", nil
		}
		return "Helium 9.9.9 (Chromium 151.0.0.0)", nil
	}
	t.Cleanup(func() { lookPath, runCmd = oldLook, oldRun })
}

// unpackHelium mimics tar: one top-level directory holding the executable.
func unpackHelium(t *testing.T) func(string) {
	t.Helper()
	return func(dir string) {
		inner := filepath.Join(dir, "helium-9.9.9-linux")
		if err := os.MkdirAll(inner, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inner, "helium"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnsureEngineUsesInstalledBrowser(t *testing.T) {
	want := filepath.Join(fakeBrowserOnPath(t, "chromium"), exeName("chromium"))
	got, installed, err := EnsureEngine(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if installed {
		t.Error("installed = true for a browser that was already there")
	}
}

func TestEnsureEngineInstallsHelium(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("provisioning covers Linux")
	}
	noBrowserAnywhere(t)
	useServer(t, releaseServer(t, linuxAssetName(t)))
	stubExec(t, unpackHelium(t))

	home := t.TempDir()
	var steps []string
	got, installed, err := EnsureEngine(context.Background(), home, func(format string, args ...any) {
		steps = append(steps, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	if !installed {
		t.Error("installed = false after provisioning")
	}
	if got != EngineBinary(home) {
		t.Errorf("path = %q, want %q", got, EngineBinary(home))
	}
	if !executable(got) {
		t.Errorf("%s is not executable", got)
	}
	if _, err := os.Stat(filepath.Join(home, "engine", ".staging")); !os.IsNotExist(err) {
		t.Error("staging directory outlived the install")
	}
	joined := strings.Join(steps, "\n")
	for _, want := range []string{"downloading Helium 9.9.9", "unpacking", "installed Helium 9.9.9"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress %q missing %q", joined, want)
		}
	}
}

func TestEnsureEngineFindsProvisionedEngine(t *testing.T) {
	home := t.TempDir()
	fakeBrowserOnPath(t)
	t.Setenv("FACTOR_HOME", home)
	if err := os.MkdirAll(EngineDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(EngineBinary(home), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, installed, err := EnsureEngine(context.Background(), home, nil)
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	if got != EngineBinary(home) || installed {
		t.Errorf("got (%q, %v), want the provisioned engine and no reinstall", got, installed)
	}
}

func TestEnsureEngineReportsMissingTar(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("provisioning covers Linux")
	}
	noBrowserAnywhere(t)
	useServer(t, releaseServer(t, linuxAssetName(t)))
	old := lookPath
	lookPath = func(string) (string, error) { return "", fmt.Errorf("not found") }
	t.Cleanup(func() { lookPath = old })

	_, _, err := EnsureEngine(context.Background(), t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "tar is needed") {
		t.Errorf("err = %v, want a missing-tar report", err)
	}
}

func TestEnsureEngineReportsUnusableInstall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("provisioning covers Linux")
	}
	noBrowserAnywhere(t)
	useServer(t, releaseServer(t, linuxAssetName(t)))
	oldLook, oldRun := lookPath, runCmd
	lookPath = func(string) (string, error) { return "/usr/bin/tar", nil }
	unpack := unpackHelium(t)
	runCmd = func(_ context.Context, argv []string) (string, error) {
		if argv[0] == "tar" {
			unpack(argv[len(argv)-1])
			return "", nil
		}
		return "Illegal instruction", fmt.Errorf("signal: illegal instruction")
	}
	t.Cleanup(func() { lookPath, runCmd = oldLook, oldRun })

	_, _, err := EnsureEngine(context.Background(), t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "will not run here") {
		t.Errorf("err = %v, want the binary to be reported unusable", err)
	}
}

func TestHeliumAssetErrors(t *testing.T) {
	t.Run("release lookup fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusInternalServerError)
		}))
		defer srv.Close()
		useServer(t, srv)
		if _, _, err := heliumAsset(context.Background()); err == nil {
			t.Error("want an error for a failed release lookup")
		}
	})

	t.Run("index is not json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "<html>")
		}))
		defer srv.Close()
		useServer(t, srv)
		if _, _, err := heliumAsset(context.Background()); err == nil {
			t.Error("want an error for an unreadable index")
		}
	})

	t.Run("no build for this architecture", func(t *testing.T) {
		useServer(t, releaseServer(t, "helium-9.9.9-sparc_linux.tar.xz"))
		_, _, err := heliumAsset(context.Background())
		if err == nil || !strings.Contains(err.Error(), "no ") {
			t.Errorf("err = %v, want a missing-build report", err)
		}
	})
}

func TestDownloadRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	err := download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f"), 0, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want the status reported", err)
	}
}

func TestProgressReaderReportsEveryTenth(t *testing.T) {
	var steps []string
	pr := &progressReader{
		r:     strings.NewReader(strings.Repeat("x", 100)),
		total: 100,
		report: func(format string, args ...any) {
			steps = append(steps, fmt.Sprintf(format, args...))
		},
	}
	buf := make([]byte, 10)
	for {
		if _, err := pr.Read(buf); err != nil {
			break
		}
	}
	if len(steps) != 10 {
		t.Fatalf("got %d reports (%v), want 10", len(steps), steps)
	}
	if steps[0] != "downloaded 10%" || steps[9] != "downloaded 100%" {
		t.Errorf("reports = %v", steps)
	}
}

func TestSingleDir(t *testing.T) {
	t.Run("no directory", func(t *testing.T) {
		if _, err := singleDir(t.TempDir()); err == nil {
			t.Error("want an error when nothing was unpacked")
		}
	})
	t.Run("several directories", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"a", "b"} {
			if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := singleDir(root); err == nil {
			t.Error("want an error for an ambiguous archive")
		}
	})
	t.Run("missing root", func(t *testing.T) {
		if _, err := singleDir(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Error("want an error for a missing root")
		}
	})
}

func TestFindBrowserBinaryUsesWellKnownPaths(t *testing.T) {
	home := t.TempDir()
	fakeBrowserOnPath(t)
	t.Setenv("FACTOR_HOME", home)
	if _, err := FindBrowserBinary(""); err == nil {
		t.Fatal("want an error before anything is provisioned")
	}
	if err := os.MkdirAll(EngineDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(EngineBinary(home), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := FindBrowserBinary("")
	if err != nil {
		t.Fatalf("FindBrowserBinary: %v", err)
	}
	if got != EngineBinary(home) {
		t.Errorf("path = %q, want the provisioned engine %q", got, EngineBinary(home))
	}
}

func TestExecutableRejectsDirectoriesAndPlainFiles(t *testing.T) {
	dir := t.TempDir()
	if executable(dir) {
		t.Error("a directory is not an executable")
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && executable(plain) {
		t.Error("a non-executable file passed")
	}
}

// A gateway started from an ssh shell has no display; a headful browser there
// does not degrade, it refuses to start.
func TestDisplayAvailableFollowsTheEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("display detection is a Linux concern")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if displayAvailable() {
		t.Error("reported a display with neither DISPLAY nor WAYLAND_DISPLAY set")
	}
	t.Setenv("DISPLAY", ":0")
	if !displayAvailable() {
		t.Error("no display reported with DISPLAY=:0")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	if !displayAvailable() {
		t.Error("no display reported under Wayland")
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"2.34", "2.34", true},
		{"2.39", "2.34", true},
		{"2.31", "2.34", false},
		{"3.0", "2.34", true},
		{"1.9", "2.34", false},
		{"2", "2.34", false},
		{"x.y", "2.34", false},
	}
	for _, c := range cases {
		if got := versionAtLeast(c.have, c.want); got != c.ok {
			t.Errorf("versionAtLeast(%q, %q) = %v", c.have, c.want, got)
		}
	}
}

func TestFastEngineSupportedReadsTheCLibrary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the C library check is a Linux concern")
	}
	old := runCmd
	t.Cleanup(func() { runCmd = old })

	runCmd = func(context.Context, []string) (string, error) {
		return "ldd (Ubuntu GLIBC 2.31-0ubuntu9) 2.31\nCopyright (C) 2020\n", nil
	}
	ok, why := FastEngineSupported()
	if ok || !strings.Contains(why, "2.31") {
		t.Errorf("ok = %v, why = %q, want a refusal naming the version found", ok, why)
	}

	runCmd = func(context.Context, []string) (string, error) {
		return "ldd (GNU libc) 2.39\n", nil
	}
	if ok, why := FastEngineSupported(); !ok {
		t.Errorf("glibc 2.39 was refused: %q", why)
	}

	runCmd = func(context.Context, []string) (string, error) {
		return "musl libc (x86_64)\nVersion 1.2.4\n", nil
	}
	if ok, why := FastEngineSupported(); ok || !strings.Contains(why, "musl") {
		t.Errorf("ok = %v, why = %q, want musl reported", ok, why)
	}

	// Undetermined must not block the attempt: the install verifies anyway.
	runCmd = func(context.Context, []string) (string, error) { return "", fmt.Errorf("no ldd") }
	if ok, _ := FastEngineSupported(); !ok {
		t.Error("an unreadable C library version blocked the install")
	}
}

func TestEnsureFastEngineRefusesBeforeDownloading(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the C library check is a Linux concern")
	}
	old := runCmd
	runCmd = func(context.Context, []string) (string, error) {
		return "ldd (Ubuntu GLIBC 2.31-0ubuntu9) 2.31\n", nil
	}
	t.Cleanup(func() { runCmd = old })

	oldAPI := releaseAPI
	releaseAPI = "http://127.0.0.1:1/%s" // any lookup here is a test failure
	t.Cleanup(func() { releaseAPI = oldAPI })

	home := t.TempDir()
	_, installed, err := EnsureFastEngine(context.Background(), home, nil)
	if installed || err == nil || !strings.Contains(err.Error(), "glibc 2.34") {
		t.Errorf("installed = %v, err = %v, want a refusal before any download", installed, err)
	}
}
