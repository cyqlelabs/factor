package memory

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpgradeMethodReadsTheInstallLayout(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		path string
		want string
	}{
		{filepath.Join(VenvDir(home), "bin", "smrti"), MethodVenv},
		{"/home/u/.local/share/uv/tools/smrti/bin/smrti", MethodUv},
		{"/home/u/.local/share/pipx/venvs/smrti/bin/smrti", MethodPipx},
		{"/home/u/.local/pipx/venvs/smrti/bin/smrti", MethodPipx},
		{"/home/u/.local/bin/smrti", MethodPip},
		{"/usr/local/bin/smrti", MethodPip},
	} {
		if got := UpgradeMethod(tc.path, home); got != tc.want {
			t.Errorf("UpgradeMethod(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestUpgradeMethodFollowsTheLinkOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("console scripts are not symlinked on Windows")
	}
	home := t.TempDir()
	venv := filepath.Join(home, "share", "pipx", "venvs", "smrti", "bin")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(venv, "smrti")
	if err := os.WriteFile(real, []byte("#!/usr/bin/python3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "smrti")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// What is on PATH says nothing; what it points at says pipx.
	if got := UpgradeMethod(link, home); got != MethodPipx {
		t.Fatalf("UpgradeMethod(%q) = %q", link, got)
	}
}

func TestUpgradeRunsTheInstallerThatOwnsTheInstall(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		name    string
		present []string
		path    string
		want    string
	}{
		{"uv", []string{"uv"}, "/home/u/.local/share/uv/tools/smrti/bin/smrti", "uv tool upgrade smrti"},
		{"pipx", []string{"pipx"}, "/home/u/.local/share/pipx/venvs/smrti/bin/smrti", "pipx upgrade smrti"},
		{"pip", []string{"pip3"}, "/home/u/.local/bin/smrti", "pip3 install --user --upgrade smrti"},
		{"venv", []string{"python3"}, filepath.Join(VenvDir(home), "bin", "smrti"), "pip install --upgrade smrti"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEnv(t, tc.present...)
			method, err := Upgrade(context.Background(), tc.path, home, nil)
			if err != nil {
				t.Fatal(err)
			}
			if method != tc.name {
				t.Errorf("method = %q, want %q", method, tc.name)
			}
			line := strings.Join(f.log, " | ")
			if !strings.Contains(line, tc.want) {
				t.Fatalf("commands %q missing %q", line, tc.want)
			}
			// The venv is already there; upgrading must not rebuild it.
			if strings.Contains(line, "-m venv") {
				t.Errorf("an existing install was reinstalled from scratch: %q", line)
			}
		})
	}
}

func TestUpgradeSaysWhenTheInstallerIsGone(t *testing.T) {
	f := newFakeEnv(t) // nothing installed on this machine
	_, err := Upgrade(context.Background(), "/home/u/.local/share/uv/tools/smrti/bin/smrti", t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "no longer on PATH") {
		t.Fatalf("error = %v", err)
	}
	if len(f.log) != 0 {
		t.Errorf("a missing installer must not be worked around with another: %v", f.log)
	}

	// Nor may a pip install be upgraded on a machine with no pip at all.
	if _, err := Upgrade(context.Background(), "/home/u/.local/bin/smrti", t.TempDir(), nil); err == nil ||
		!strings.Contains(err.Error(), "not available here") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpgradeRetriesAnExternallyManagedPython(t *testing.T) {
	home := t.TempDir()
	f := newFakeEnv(t, "pip3")
	f.fail("pip3 install --user --upgrade smrti", "error: externally-managed-environment")

	var steps []string
	if _, err := Upgrade(context.Background(), "/home/u/.local/bin/smrti", home,
		func(format string, args ...any) { steps = append(steps, format) }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.log, " | "), "--break-system-packages") {
		t.Fatalf("commands = %v", f.log)
	}
	if len(steps) != 2 {
		t.Errorf("both attempts should be reported: %v", steps)
	}
}

func TestUpgradeReportsAFailedInstaller(t *testing.T) {
	f := newFakeEnv(t, "pipx")
	f.fail("pipx upgrade smrti", "No apps associated with package smrti")
	_, err := Upgrade(context.Background(), "/home/u/.local/share/pipx/venvs/smrti/bin/smrti", t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "No apps associated") {
		t.Fatalf("the installer's own words are the diagnosis: %v", err)
	}
}

func TestInstalledVersionAsksTheInterpreterBesideTheScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this fakes a unix venv layout")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "smrti")
	if err := os.WriteFile(exe, []byte("#!"+bin+"/python\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	f := newFakeEnv(t)
	f.results[filepath.Join(bin, "python3")+" -c "+versionScript] = struct {
		out string
		err error
	}{"0.12.2\n", nil}

	if got := InstalledVersion(context.Background(), exe); got != "0.12.2" {
		t.Fatalf("version = %q (ran %v)", got, f.log)
	}
}

func TestInstalledVersionFallsBackToTheShebangThenPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("console scripts are executables on Windows, with no shebang to read")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "smrti") // a pip --user script: no interpreter beside it
	if err := os.WriteFile(exe, []byte("#!/usr/bin/env python3.12\nimport smrti\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	f := newFakeEnv(t, "python3")
	// The shebang's interpreter is not on this machine; the one on PATH is.
	f.fail("python3.12 -c "+versionScript, "no such file")
	f.results["python3 -c "+versionScript] = struct {
		out string
		err error
	}{"0.11.3\n", nil}

	if got := InstalledVersion(context.Background(), exe); got != "0.11.3" {
		t.Fatalf("version = %q (ran %v)", got, f.log)
	}
	if !strings.Contains(strings.Join(f.log, " | "), "python3.12") {
		t.Errorf("the script's own interpreter was never asked: %v", f.log)
	}
}

func TestInstalledVersionIgnoresOutputThatIsNotAVersion(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "smrti")
	if err := os.WriteFile(exe, []byte("script"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFakeEnv(t, "python3")
	f.results["python3 -c "+versionScript] = struct {
		out string
		err error
	}{"Traceback (most recent call last)\n", nil}
	if got := InstalledVersion(context.Background(), exe); got != "" {
		t.Fatalf("version = %q", got)
	}
}
