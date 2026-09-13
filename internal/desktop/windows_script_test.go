package desktop

import (
	"context"
	"strings"
	"testing"
)

// scriptedWindows builds the Windows controller against a recorder, so the
// PowerShell it generates can be read on any machine.
func scriptedWindows(out string) (*windowsController, *[]string) {
	var scripts []string
	env := Env{
		GOOS: "windows",
		Has:  func(bin string) bool { return bin == "powershell" },
		Run: func(_ context.Context, _ string, argv ...string) (string, error) {
			scripts = append(scripts, argv[len(argv)-1])
			return out, nil
		},
	}
	return &windowsController{env: env}, &scripts
}

// Windows addresses the desktop from the primary monitor, so a screen placed
// above or to the left of it sits at negative coordinates. Factor addresses it
// from the top-left of what it captured. Every coordinate crossing that
// boundary has to be converted, or a click computed from the frame lands on
// the wrong monitor.
func TestWindowsCoordinatesAreRelativeToTheCapture(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		what string
		call func(c *windowsController)
	}{
		{"the pointer", func(c *windowsController) { _ = c.MoveMouse(ctx, 40, 60) }},
		{"a click at a point", func(c *windowsController) { _ = c.Click(ctx, "left", 1, &Point{X: 40, Y: 60}) }},
		{"moving a window", func(c *windowsController) {
			_ = c.MoveResize(ctx, Window{ID: "7"}, Geometry{X: 40, Y: 60, W: 100, H: 80, HasPos: true, HasSize: true})
		}},
		{"a region screenshot", func(c *windowsController) {
			_ = c.Screenshot(ctx, "shot.png", Shot{Mode: "region", Region: Geometry{X: 40, Y: 60, W: 100, H: 80}})
		}},
	} {
		c, scripts := scriptedWindows("")
		tc.call(c)
		if len(*scripts) == 0 {
			t.Fatalf("%s ran nothing", tc.what)
		}
		script := (*scripts)[len(*scripts)-1]
		if !strings.Contains(script, "$vs = [System.Windows.Forms.SystemInformation]::VirtualScreen") {
			t.Errorf("%s: the script never asks where the desktop starts:\n%s", tc.what, script)
		}
		if !strings.Contains(script, "40 + $vs.Left") || !strings.Contains(script, "60 + $vs.Top") {
			t.Errorf("%s: coordinates were not converted:\n%s", tc.what, script)
		}
	}
}

// A window the user dragged onto a second screen was never in the frame, so
// the model was told the desktop was empty.
func TestWindowsCapturesEveryMonitor(t *testing.T) {
	c, scripts := scriptedWindows("")
	_ = c.Screenshot(context.Background(), "shot.png", Shot{Mode: "screen"})
	script := (*scripts)[0]
	if strings.Contains(script, "PrimaryScreen") {
		t.Errorf("only the primary monitor is captured:\n%s", script)
	}
	if !strings.Contains(script, "$b = $vs") {
		t.Errorf("the capture does not cover the whole desktop:\n%s", script)
	}
}

func TestWindowsScreenSizeReportsTheWholeDesktop(t *testing.T) {
	c, scripts := scriptedWindows("3840 1080\n")
	w, h, err := c.ScreenSize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w != 3840 || h != 1080 {
		t.Fatalf("ScreenSize = %dx%d, want 3840x1080", w, h)
	}
	if strings.Contains((*scripts)[0], "PrimaryScreen") {
		t.Error("the size reported is one monitor's, not the desktop's")
	}
}

// Window geometry is reported in the same space the capture is, so a cell the
// model reads off a window rect and a cell it reads off the frame agree.
func TestWindowsListReportsCaptureRelativeGeometry(t *testing.T) {
	c, scripts := scriptedWindows("66\t12\tnotepad\t-1920\t0\t800\t600\tUntitled\n")
	wins, err := c.ListWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(wins) != 1 || wins[0].X != -1920 {
		t.Fatalf("windows = %+v", wins)
	}
	if !strings.Contains((*scripts)[0], "$r.Left - $vs.Left") {
		t.Errorf("geometry is reported in Windows' own origin:\n%s", (*scripts)[0])
	}
}
