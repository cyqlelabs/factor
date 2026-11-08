package desktop

import (
	"fmt"
	"image"
	"image/color"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// xServer is one X server running on this machine, read off the process
// table. A machine can run several: the console's Xorg, an Xvfb somebody
// started for a headless job, a VNC server. Only the first is the screen a
// person is looking at, and a process told DISPLAY=:0 cannot tell which one
// it was given.
type xServer struct {
	Display string // ":1"
	Program string // "Xorg", "Xvfb"
	VT      string // "vt1", or "" for a server on no console
	Auth    string // the -auth file the server reads its cookies from
	// Virtual is a server that draws into memory or into another server's
	// window rather than onto a monitor: nobody sees what it shows.
	Virtual bool
}

// virtualServers draw where no person is looking.
var virtualServers = map[string]bool{
	"Xvfb": true, "Xvnc": true, "Xtigervnc": true, "Xephyr": true, "Xnest": true,
}

// xServerPrograms are the binaries an X server runs as.
var xServerPrograms = map[string]bool{
	"X": true, "Xorg": true, "Xorg.bin": true, "Xwayland": true,
	"Xvfb": true, "Xvnc": true, "Xtigervnc": true, "Xephyr": true, "Xnest": true,
}

var (
	displayArg = regexp.MustCompile(`^:\d+$`)
	vtArg      = regexp.MustCompile(`^vt\d+$`)
)

// xServers lists the X servers in the process table, lowest display first.
// It is Linux only, and empty wherever the table cannot be read.
func xServers(env Env) []xServer {
	if env.GOOS != "linux" || env.Glob == nil || env.ReadFile == nil {
		return nil
	}
	cmdlines, err := env.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		return nil
	}
	var out []xServer
	seen := map[string]bool{}
	for _, f := range cmdlines {
		raw, err := env.ReadFile(f)
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		program := path.Base(args[0])
		if !xServerPrograms[program] {
			continue
		}
		s := xServer{Program: program, Virtual: virtualServers[program]}
		for i, a := range args[1:] {
			switch {
			case displayArg.MatchString(a) && s.Display == "":
				s.Display = a
			case vtArg.MatchString(a):
				s.VT = a
			case a == "-auth" && i+2 < len(args):
				s.Auth = args[i+2]
			}
		}
		if s.Display == "" || seen[s.Display] {
			continue
		}
		seen[s.Display] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return displayNumber(out[i].Display) < displayNumber(out[j].Display) })
	return out
}

func displayNumber(d string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(d, ":"))
	return n
}

// serverFor is the server behind a DISPLAY value (":0", ":0.0"), if listed.
func serverFor(servers []xServer, display string) (xServer, bool) {
	num, _, _ := strings.Cut(display, ".")
	for _, s := range servers {
		if s.Display == num {
			return s, true
		}
	}
	return xServer{}, false
}

// describe names a server the way the model needs it: what it is, and
// whether anybody can see it.
func (s xServer) describe() string {
	switch {
	case s.Virtual:
		return fmt.Sprintf("%s is %s, a virtual screen nobody sees", s.Display, s.Program)
	case s.VT != "":
		return fmt.Sprintf("%s is %s on %s, the console", s.Display, s.Program, s.VT)
	}
	return fmt.Sprintf("%s is %s", s.Display, s.Program)
}

// flatColor reports whether every pixel of img is the same colour, and which.
// A capture like that has nothing drawn in it: a blanked or locked screen, or
// a display nobody draws on.
func flatColor(img *image.RGBA) (color.RGBA, bool) {
	b := img.Bounds()
	if b.Empty() {
		return color.RGBA{}, false
	}
	first := img.RGBAAt(b.Min.X, b.Min.Y)
	want := [4]byte{first.R, first.G, first.B, first.A}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := img.Pix[img.PixOffset(b.Min.X, y):img.PixOffset(b.Max.X, y)]
		for i := 0; i < len(row); i += 4 {
			if row[i] != want[0] || row[i+1] != want[1] || row[i+2] != want[2] || row[i+3] != want[3] {
				return color.RGBA{}, false
			}
		}
	}
	return first, true
}

// blankNote is what a flat capture is told: the fact, the honest reasons for
// it, and — where the machine runs more than one X server — which display is
// the console, since a screen read from the wrong server is exactly this.
func blankNote(env Env, c color.RGBA) string {
	note := fmt.Sprintf(" Every pixel of this capture is the same colour (#%02x%02x%02x): nothing is drawn on this screen. "+
		"The display may be blanked or locked", c.R, c.G, c.B)
	display := env.env("DISPLAY")
	servers := xServers(env)
	if len(servers) < 2 {
		return note + ", or the screen in front of the user is not the one this process captures (DISPLAY=" + display + ")."
	}
	described := make([]string, len(servers))
	for i, s := range servers {
		described[i] = s.describe()
	}
	return note + ", or DISPLAY=" + display + " is not the screen in front of the user. X servers on this machine: " +
		strings.Join(described, "; ") + ". Say which display was captured rather than working around the tool."
}
