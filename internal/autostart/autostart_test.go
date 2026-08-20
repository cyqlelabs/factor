package autostart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/desktop"
)

// fakeEnv is a scripted machine: systemctl answers state, every command is
// recorded, and the config dir lands in a throwaway directory.
func fakeEnv(t *testing.T, goos, systemdState string) (desktop.Env, *[][]string, string) {
	t.Helper()
	confDir := t.TempDir()
	var calls [][]string
	env := desktop.Env{
		GOOS: goos,
		Has:  func(bin string) bool { return bin == "systemctl" && systemdState != "" },
		Getenv: func(key string) string {
			if key == "XDG_CONFIG_HOME" {
				return confDir
			}
			return ""
		},
		Run: func(_ context.Context, _ string, argv ...string) (string, error) {
			calls = append(calls, argv)
			if len(argv) > 2 && argv[2] == "is-system-running" {
				if systemdState == "degraded" {
					return "degraded\n", errors.New("systemctl: exit status 1")
				}
				return systemdState + "\n", nil
			}
			return "", nil
		},
	}
	return env, &calls, confDir
}

func called(calls [][]string, want ...string) bool {
	for _, argv := range calls {
		if strings.Join(argv, " ") == strings.Join(want, " ") {
			return true
		}
	}
	return false
}

func TestInstallPrefersSystemdUserService(t *testing.T) {
	env, calls, confDir := fakeEnv(t, "linux", "running")

	entry, err := Install(context.Background(), env, "/opt/fac tor/factor", "/etc/factor.json")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Mechanism != "systemd user service" {
		t.Errorf("mechanism = %q", entry.Mechanism)
	}
	unit, err := os.ReadFile(filepath.Join(confDir, "systemd", "user", unitName))
	if err != nil {
		t.Fatal(err)
	}
	// systemd supervises: the gateway must run in the foreground, with the
	// spaced path quoted and the config named.
	want := `ExecStart="/opt/fac tor/factor" gateway -c "/etc/factor.json"`
	if !strings.Contains(string(unit), want) {
		t.Errorf("unit %q is missing %q", unit, want)
	}
	if strings.Contains(string(unit), "-d") {
		t.Error("a supervised service must not self-detach")
	}
	if !called(*calls, "systemctl", "--user", "daemon-reload") ||
		!called(*calls, "systemctl", "--user", "enable", unitName) {
		t.Errorf("service not enabled: %v", *calls)
	}

	if got, ok := Installed(env); !ok || got.Mechanism != entry.Mechanism {
		t.Errorf("Installed = (%+v, %v) after install", got, ok)
	}
}

func TestInstallAcceptsDegradedSystemd(t *testing.T) {
	env, _, confDir := fakeEnv(t, "linux", "degraded")
	entry, err := Install(context.Background(), env, "/usr/bin/factor", "")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Mechanism != "systemd user service" {
		t.Errorf("a degraded user manager still runs services, got %q", entry.Mechanism)
	}
	if unit, err := os.ReadFile(filepath.Join(confDir, "systemd", "user", unitName)); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(unit), "-c") {
		t.Errorf("no config was named, yet the unit says %q", unit)
	}
}

func TestInstallFallsBackToXDGWithoutSystemd(t *testing.T) {
	env, _, confDir := fakeEnv(t, "linux", "")

	entry, err := Install(context.Background(), env, "/usr/bin/factor", "")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Mechanism != "XDG autostart entry" {
		t.Errorf("mechanism = %q", entry.Mechanism)
	}
	data, err := os.ReadFile(filepath.Join(confDir, "autostart", desktopName))
	if err != nil {
		t.Fatal(err)
	}
	// Nothing supervises an autostart entry: the gateway detaches itself.
	if !strings.Contains(string(data), `Exec="/usr/bin/factor" gateway -d`) {
		t.Errorf("desktop entry %q lacks the detached gateway command", data)
	}
	if got, ok := Installed(env); !ok || got.Mechanism != entry.Mechanism {
		t.Errorf("Installed = (%+v, %v) after install", got, ok)
	}
}

func TestInstallSkipsSystemdWhoseBusIsGone(t *testing.T) {
	// systemctl exists but cannot reach a user bus: stdout stays empty.
	env, _, _ := fakeEnv(t, "linux", "")
	env.Has = func(bin string) bool { return bin == "systemctl" }
	env.Run = func(context.Context, string, ...string) (string, error) {
		return "", errors.New("Failed to connect to bus")
	}
	entry, err := Install(context.Background(), env, "/usr/bin/factor", "")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Mechanism != "XDG autostart entry" {
		t.Errorf("mechanism = %q, want the XDG fallback", entry.Mechanism)
	}
}

func TestUninstallRemovesEveryLinuxFlavour(t *testing.T) {
	env, calls, confDir := fakeEnv(t, "linux", "running")
	if _, err := Install(context.Background(), env, "/usr/bin/factor", ""); err != nil {
		t.Fatal(err)
	}
	// An older XDG entry left behind by a pre-systemd install goes too.
	if err := writeFile(filepath.Join(confDir, "autostart", desktopName), "stale"); err != nil {
		t.Fatal(err)
	}

	if err := Uninstall(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !called(*calls, "systemctl", "--user", "disable", unitName) {
		t.Errorf("service not disabled: %v", *calls)
	}
	for _, path := range []string{
		filepath.Join(confDir, "systemd", "user", unitName),
		filepath.Join(confDir, "autostart", desktopName),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived the uninstall", path)
		}
	}
	if _, ok := Installed(env); ok {
		t.Error("Installed still true after uninstall")
	}
}

func TestUninstallWithNothingInstalledIsQuiet(t *testing.T) {
	env, _, _ := fakeEnv(t, "linux", "")
	if err := Uninstall(context.Background(), env); err != nil {
		t.Errorf("Uninstall = %v with nothing installed", err)
	}
}

func TestInstallLaunchdAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FACTOR_HOME", filepath.Join(home, ".factor"))
	env, calls, _ := fakeEnv(t, "darwin", "")

	entry, err := Install(context.Background(), env, "/Applications/fac&tor", "")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Mechanism != "launchd agent" {
		t.Errorf("mechanism = %q", entry.Mechanism)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", plistName)
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>/Applications/fac&amp;tor</string>", // XML-escaped
		"<string>gateway</string>",
		"<key>RunAtLoad</key>",
		filepath.Join(home, ".factor", "gateway.log"),
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("plist is missing %q:\n%s", want, data)
		}
	}
	if !called(*calls, "launchctl", "load", "-w", plist) {
		t.Errorf("agent not loaded: %v", *calls)
	}

	if got, ok := Installed(env); !ok || got.Mechanism != "launchd agent" {
		t.Errorf("Installed = (%+v, %v) after install", got, ok)
	}
	if err := Uninstall(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !called(*calls, "launchctl", "unload", plist) {
		t.Errorf("agent not unloaded: %v", *calls)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Error("plist survived the uninstall")
	}
}

func TestWindowsDispatchReachesTheRegistryStub(t *testing.T) {
	env, _, _ := fakeEnv(t, "windows", "")
	if _, err := Install(context.Background(), env, `C:\factor.exe`, ""); err == nil {
		t.Error("the non-windows registry stub reported success")
	}
	if _, ok := Installed(env); ok {
		t.Error("Installed = true through the non-windows stub")
	}
	if err := Uninstall(context.Background(), env); err == nil {
		t.Error("Uninstall through the stub reported success")
	}
}

func TestInstallSurfacesSystemctlFailure(t *testing.T) {
	env, _, _ := fakeEnv(t, "linux", "running")
	inner := env.Run
	env.Run = func(ctx context.Context, stdin string, argv ...string) (string, error) {
		if len(argv) > 2 && argv[2] == "enable" {
			return "", errors.New("Failed to enable unit")
		}
		return inner(ctx, stdin, argv...)
	}
	if _, err := Install(context.Background(), env, "/usr/bin/factor", ""); err == nil ||
		!strings.Contains(err.Error(), "enable") {
		t.Errorf("Install = %v, want the systemctl failure", err)
	}
}

// Where the login entry lands is decided by XDG_CONFIG_HOME when the session
// sets one. A machine that keeps its config elsewhere would otherwise get an
// autostart entry in a directory nothing reads — an install that reports
// success and never starts anything.
func TestConfigDirFollowsXDGConfigHome(t *testing.T) {
	env := desktop.Env{Getenv: func(key string) string {
		if key == "XDG_CONFIG_HOME" {
			return "/custom/config"
		}
		return ""
	}}
	if got := configDir(env); got != "/custom/config" {
		t.Errorf("configDir = %q, want the XDG override", got)
	}
}

// Without the variable the entry belongs under the user's own home, which is
// where every desktop looks by default.
func TestConfigDirFallsBackToTheUsersHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this machine: %v", err)
	}
	want := filepath.Join(home, ".config")

	unset := desktop.Env{Getenv: func(string) string { return "" }}
	if got := configDir(unset); got != want {
		t.Errorf("configDir with an empty XDG_CONFIG_HOME = %q, want %q", got, want)
	}
	// An Env that cannot read the environment at all must still answer.
	if got := configDir(desktop.Env{}); got != want {
		t.Errorf("configDir without a Getenv = %q, want %q", got, want)
	}
}
