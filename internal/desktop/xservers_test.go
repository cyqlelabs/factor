package desktop

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
)

// procTable scripts a Linux process table: pid → argv.
func procTable(procs map[string][]string) Env {
	return Env{
		GOOS: "linux",
		Glob: func(pattern string) ([]string, error) {
			if pattern != "/proc/[0-9]*/cmdline" {
				return nil, nil
			}
			var out []string
			for pid := range procs {
				out = append(out, "/proc/"+pid+"/cmdline")
			}
			return out, nil
		},
		ReadFile: func(name string) ([]byte, error) {
			pid := strings.TrimSuffix(strings.TrimPrefix(name, "/proc/"), "/cmdline")
			argv, ok := procs[pid]
			if !ok {
				return nil, errors.New("gone")
			}
			return []byte(strings.Join(argv, "\x00") + "\x00"), nil
		},
	}
}

// The Puppy box: an Xvfb on :0 nobody looks at, and the console's Xorg on :1.
var puppyBox = map[string][]string{
	"10463": {"Xvfb", ":0", "-screen", "0", "1920x1080x24"},
	"32179": {"/usr/lib/xorg/Xorg", ":1", "vt1", "-keeptty", "-auth", "/tmp/serverauth.0ltkUg0Thi"},
	"5687":  {"/root/.local/bin/factor", "gateway"},
	"77":    {"Xorg", "-nolisten", "tcp"},
	"99":    {},
}

func TestXServersReadsTheProcessTable(t *testing.T) {
	got := xServers(procTable(puppyBox))
	if len(got) != 2 {
		t.Fatalf("servers = %+v", got)
	}
	if s := got[0]; s.Display != ":0" || s.Program != "Xvfb" || !s.Virtual || s.VT != "" {
		t.Errorf(":0 = %+v", s)
	}
	if s := got[1]; s.Display != ":1" || s.Program != "Xorg" || s.Virtual || s.VT != "vt1" || s.Auth != "/tmp/serverauth.0ltkUg0Thi" {
		t.Errorf(":1 = %+v", s)
	}
}

func TestXServersReadsNothingItCannot(t *testing.T) {
	env := procTable(puppyBox)
	env.GOOS = "darwin"
	if got := xServers(env); got != nil {
		t.Errorf("darwin listed %+v", got)
	}
	env = procTable(puppyBox)
	env.ReadFile = nil
	if got := xServers(env); got != nil {
		t.Errorf("no ReadFile listed %+v", got)
	}
	env = procTable(puppyBox)
	env.Glob = func(string) ([]string, error) { return nil, errors.New("no /proc") }
	if got := xServers(env); got != nil {
		t.Errorf("an unreadable table listed %+v", got)
	}
}

func TestFlatColor(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	if c, flat := flatColor(img); !flat || c != (color.RGBA{}) {
		t.Errorf("a black frame: %v %v", c, flat)
	}
	img.SetRGBA(7, 5, color.RGBA{1, 0, 0, 255})
	if _, flat := flatColor(img); flat {
		t.Error("a frame with one lit pixel read as flat")
	}
	if _, flat := flatColor(img.SubImage(image.Rect(0, 0, 4, 4)).(*image.RGBA)); !flat {
		t.Error("a flat corner of a frame read as drawn")
	}
	if _, flat := flatColor(image.NewRGBA(image.Rectangle{})); flat {
		t.Error("an empty frame read as flat")
	}
}

func TestBlankNoteNamesTheConsole(t *testing.T) {
	env := procTable(puppyBox)
	env.Getenv = func(k string) string { return map[string]string{"DISPLAY": ":0"}[k] }
	note := blankNote(env, color.RGBA{A: 255})
	for _, want := range []string{"#000000", "DISPLAY=:0", ":0 is Xvfb, a virtual screen nobody sees", ":1 is Xorg on vt1, the console"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q: %s", want, note)
		}
	}
	lone := blankNote(Env{Getenv: env.Getenv}, color.RGBA{A: 255})
	if !strings.Contains(lone, "blanked or locked") || strings.Contains(lone, "X servers") {
		t.Errorf("one server, note = %s", lone)
	}
}

// drawnMachine writes a frame with something on it.
func drawnMachine(t *testing.T) *fakeMachine {
	t.Helper()
	m := newMachine("linux", "scrot", "xdotool")
	m.onRun = func(argv []string) {
		if argv[0] != "scrot" {
			return
		}
		img := image.NewRGBA(image.Rect(0, 0, 320, 200))
		img.SetRGBA(10, 10, color.RGBA{255, 255, 255, 255})
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(argv[len(argv)-1], buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// A capture with nothing drawn in it says so, in both tools that take one.
func TestCapturesSayWhenNothingIsDrawn(t *testing.T) {
	flat, _ := newTools(t, shotMachine(t, 320, 200))
	drawn, _ := newTools(t, drawnMachine(t))
	for _, name := range []string{"screen_view", "screenshot"} {
		if res := run(t, flat[name], map[string]any{}); res.IsError || !strings.Contains(res.ForLLM, "same colour (#285078)") {
			t.Errorf("%s on a flat frame: %s", name, res.ForLLM)
		}
		if res := run(t, drawn[name], map[string]any{}); res.IsError || strings.Contains(res.ForLLM, "same colour") {
			t.Errorf("%s on a drawn frame: %s", name, res.ForLLM)
		}
	}
}

// puppyEnv is the Puppy box as a process sees it: the process table, the two
// X sockets, the console's auth file, and whatever DISPLAY it was started with.
func puppyEnv(display string, sockets ...string) (Env, map[string]string) {
	env := procTable(puppyBox)
	procs := env.Glob
	read := env.ReadFile
	set := map[string]string{"DISPLAY": display}
	env.Getenv = func(k string) string { return set[k] }
	env.Glob = func(pattern string) ([]string, error) {
		if strings.HasPrefix(pattern, "/proc/") {
			return procs(pattern)
		}
		var out []string
		for _, s := range sockets {
			if pattern == x11SocketDir+"/X*" || pattern == s {
				out = append(out, s)
			}
		}
		return out, nil
	}
	env.ReadFile = func(name string) ([]byte, error) {
		if name == "/tmp/serverauth.0ltkUg0Thi" {
			return []byte("cookie"), nil
		}
		return read(name)
	}
	return env, set
}

func setter(set map[string]string) func(k, v string) error {
	return func(k, v string) error { set[k] = v; return nil }
}

// A gateway a script started on the Xvfb moves to the console at startup,
// and takes the console's cookies with it.
func TestAdoptConsoleMovesOffTheVirtualScreen(t *testing.T) {
	env, set := puppyEnv(":0", "/tmp/.X11-unix/X0", "/tmp/.X11-unix/X1")
	got := AdoptConsole(env, setter(set))
	if !strings.Contains(got, "DISPLAY=:1 (was :0: :0 is Xvfb") {
		t.Fatalf("AdoptConsole = %q", got)
	}
	if set["DISPLAY"] != ":1" || set["XAUTHORITY"] != "/tmp/serverauth.0ltkUg0Thi" {
		t.Errorf("env = %v", set)
	}
}

func TestAdoptConsoleLeavesEverythingElseAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		display string
		sockets []string
		goos    string
	}{
		"already on the console":    {":1", []string{"/tmp/.X11-unix/X0", "/tmp/.X11-unix/X1"}, "linux"},
		"the console's socket gone": {":0", []string{"/tmp/.X11-unix/X0"}, "linux"},
		"a display nothing lists":   {":7", []string{"/tmp/.X11-unix/X7"}, "linux"},
		"no display at all":         {"", nil, "linux"},
		"not linux":                 {":0", []string{"/tmp/.X11-unix/X0", "/tmp/.X11-unix/X1"}, "darwin"},
	} {
		env, set := puppyEnv(tc.display, tc.sockets...)
		env.GOOS = tc.goos
		if got := AdoptConsole(env, setter(set)); got != "" || set["DISPLAY"] != tc.display {
			t.Errorf("%s: AdoptConsole = %q, env = %v", name, got, set)
		}
	}
	env, _ := puppyEnv(":0", "/tmp/.X11-unix/X0", "/tmp/.X11-unix/X1")
	if got := AdoptConsole(env, nil); got != "" {
		t.Errorf("no setter, yet %q", got)
	}
}

// With no DISPLAY at all, the console is adopted over a lower-numbered
// virtual screen; with no process table to read, the lowest socket still is.
func TestFindDisplayPrefersTheConsole(t *testing.T) {
	env, _ := puppyEnv("", "/tmp/.X11-unix/X0", "/tmp/.X11-unix/X1")
	if key, value := findDisplay(env); key != "DISPLAY" || value != ":1" {
		t.Errorf("findDisplay = %s=%s, want DISPLAY=:1", key, value)
	}
	env.ReadFile = nil
	if _, value := findDisplay(env); value != ":0" {
		t.Errorf("without a process table, findDisplay = %s, want :0", value)
	}
	env, _ = puppyEnv("", "/tmp/.X11-unix/X0")
	if _, value := findDisplay(env); value != ":0" {
		t.Errorf("with only the virtual screen up, findDisplay = %s, want :0", value)
	}
}

// A process with no DISPLAY adopts the console with its cookies, wherever
// it can read them.
func TestAdoptDisplayBringsTheConsolesCookies(t *testing.T) {
	env, set := puppyEnv("", "/tmp/.X11-unix/X0", "/tmp/.X11-unix/X1")
	if got := AdoptDisplay(env, setter(set)); got != "DISPLAY=:1" {
		t.Fatalf("AdoptDisplay = %q", got)
	}
	if set["XAUTHORITY"] != "/tmp/serverauth.0ltkUg0Thi" {
		t.Errorf("env = %v", set)
	}
}
