//go:build linux

package desktop

import (
	"context"
	"errors"
	"fmt"
	"image"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeObj is one scripted AT-SPI object.
type fakeObj struct {
	name, roleName string
	role           uint32
	states         []int
	ifaces         []string
	screen, window image.Rectangle
	actions        []string
	text           string
	value          float64
	children       []dbus.ObjectPath
	labelledBy     dbus.ObjectPath
	id             string
	doAction       func(i int32) bool
	setText        func(s string) bool
}

// fakeATSPI answers the accessibility bus from a scripted tree: one
// application, ":1.9", whose objects live at their paths.
type fakeATSPI struct {
	mu      sync.Mutex
	objs    map[dbus.ObjectPath]*fakeObj
	apps    []atspiRef
	pidOf   int
	failAll error
	// hang names applications whose calls never answer: a frozen program.
	hang   map[string]bool
	calls  []string
	closed bool
}

const fakeApp = ":1.9"

func (f *fakeATSPI) pid(context.Context, string) (int, error) { return f.pidOf, nil }
func (f *fakeATSPI) close()                                   { f.closed = true }

func stateWords(bits []int) []uint32 {
	w := make([]uint32, 2)
	for _, b := range bits {
		w[b/32] |= 1 << (b % 32)
	}
	return w
}

func (f *fakeATSPI) call(ctx context.Context, ref atspiRef, method string, args []any, out ...any) error {
	f.mu.Lock()
	f.calls = append(f.calls, string(ref.Path)+" "+method)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.failAll != nil {
		return f.failAll
	}
	if f.hang[ref.Name] {
		<-ctx.Done()
		return ctx.Err()
	}
	if ref.Name == atspiRegistry {
		*out[0].(*[]atspiRef) = f.apps
		return nil
	}
	o, ok := f.objs[ref.Path]
	if !ok {
		return dbus.Error{Name: "org.freedesktop.DBus.Error.UnknownObject"}
	}
	switch strings.TrimPrefix(strings.TrimPrefix(method, ifaceAccessible+"."), "org.a11y.atspi.") {
	case ifaceProps + ".GetAll":
		*out[0].(*map[string]dbus.Variant) = map[string]dbus.Variant{
			"Name": dbus.MakeVariant(o.name), "Description": dbus.MakeVariant(""),
			"ChildCount": dbus.MakeVariant(int32(len(o.children))), "AccessibleId": dbus.MakeVariant(o.id),
		}
	case ifaceProps + ".Get":
		if args[1] == "CurrentValue" {
			*out[0].(*dbus.Variant) = dbus.MakeVariant(o.value)
		} else {
			*out[0].(*dbus.Variant) = dbus.MakeVariant(o.name)
		}
	case "GetRole":
		*out[0].(*uint32) = o.role
	case "GetRoleName":
		*out[0].(*string) = o.roleName
	case "GetState":
		*out[0].(*[]uint32) = stateWords(o.states)
	case "GetInterfaces":
		*out[0].(*[]string) = append([]string{ifaceAccessible}, o.ifaces...)
	case "GetChildren":
		var kids []atspiRef
		for _, c := range o.children {
			kids = append(kids, atspiRef{Name: fakeApp, Path: c})
		}
		*out[0].(*[]atspiRef) = kids
	case "GetRelationSet":
		rels := out[0].(*[]struct {
			Type    uint32
			Targets []atspiRef
		})
		if o.labelledBy != "" {
			*rels = append(*rels, struct {
				Type    uint32
				Targets []atspiRef
			}{relationLabelledBy, []atspiRef{{Name: fakeApp, Path: o.labelledBy}}})
		}
	case "Component.GetExtents":
		r := o.screen
		if args[0] == coordWindow {
			r = o.window
		}
		*out[0].(*atspiRect) = atspiRect{X: int32(r.Min.X), Y: int32(r.Min.Y), W: int32(r.Dx()), H: int32(r.Dy())}
	case "Action.GetActions":
		acts := out[0].(*[]struct{ Name, Description, Keys string })
		for _, a := range o.actions {
			*acts = append(*acts, struct{ Name, Description, Keys string }{Name: a})
		}
	case "Text.GetText":
		*out[0].(*string) = o.text
	case "Action.DoAction":
		*out[0].(*bool) = o.doAction == nil || o.doAction(args[0].(int32))
	case "EditableText.SetTextContents":
		ok := o.setText == nil || o.setText(args[0].(string))
		if ok {
			o.text = args[0].(string)
		}
		*out[0].(*bool) = ok
	case "Component.GrabFocus":
		*out[0].(*bool) = true
	default:
		return fmt.Errorf("unscripted %s", method)
	}
	return nil
}

var showing = []int{stShowing, stVisible, stEnabled, stSensitive}

// fixtureBus scripts the same window the live fixture draws: a frame holding
// a labelled field, a checkbox, two buttons, an expander and a slider, plus a
// modal dialog above part of it.
func fixtureBus() *fakeATSPI {
	app := dbus.ObjectPath("/org/a11y/atspi/accessible/root")
	objs := map[dbus.ObjectPath]*fakeObj{
		app: {name: "editor", roleName: "application", role: 75, children: []dbus.ObjectPath{"/frame", "/dialog", "/closed"}},
		"/frame": {name: "Notes", roleName: "frame", role: 23, states: append([]int{stActive}, showing...),
			ifaces: []string{ifaceComponent}, screen: image.Rect(100, 100, 600, 500), window: image.Rect(0, 0, 500, 400),
			children: []dbus.ObjectPath{"/filler"}},
		"/filler": {roleName: "filler", role: 20, states: showing,
			children: []dbus.ObjectPath{"/label", "/entry", "/check", "/save", "/hidden", "/tree", "/slider", "/covered"}},
		"/label": {name: "Title", roleName: "label", role: 29, states: showing, ifaces: []string{ifaceComponent},
			screen: image.Rect(110, 110, 160, 130)},
		"/entry": {roleName: "text", role: 61, states: append([]int{stEditable, stFocusable, stFocused}, showing...),
			ifaces: []string{ifaceComponent, ifaceAction, ifaceText, ifaceEditable}, actions: []string{"activate"},
			screen: image.Rect(170, 105, 400, 135), text: "draft", labelledBy: "/label"},
		"/check": {name: "Wrap", roleName: "check box", role: 7, states: append([]int{stFocusable, stChecked}, showing...),
			ifaces: []string{ifaceComponent, ifaceAction}, actions: []string{"click"}, screen: image.Rect(170, 150, 260, 170)},
		"/save": {name: "Save", roleName: "push button", role: 43, states: append([]int{stFocusable, stDefault}, showing...),
			ifaces: []string{ifaceComponent, ifaceAction}, actions: []string{"click"}, screen: image.Rect(450, 440, 550, 480), id: "save-btn"},
		"/hidden": {name: "Ghost", roleName: "push button", role: 43, states: []int{stEnabled},
			ifaces: []string{ifaceAction}, actions: []string{"click"}, children: []dbus.ObjectPath{"/never"}},
		"/tree": {name: "Folders", roleName: "tree item", role: 65, states: append([]int{stExpandable}, showing...),
			ifaces: []string{ifaceComponent, ifaceAction}, actions: []string{"expand or contract"}, screen: image.Rect(110, 200, 250, 220)},
		"/slider": {name: "Zoom", roleName: "slider", role: 51, states: showing, ifaces: []string{ifaceComponent, ifaceValue},
			screen: image.Rect(110, 240, 300, 260), value: 1.5},
		"/covered": {name: "Behind", roleName: "push button", role: 43, states: showing, ifaces: []string{ifaceComponent, ifaceAction},
			actions: []string{"click"}, screen: image.Rect(300, 300, 340, 320)},
		"/dialog": {name: "Confirm", roleName: "dialog", role: 16, states: append([]int{stModal}, showing...),
			ifaces: []string{ifaceComponent}, screen: image.Rect(280, 280, 420, 360), children: []dbus.ObjectPath{"/ok"}},
		"/ok": {name: "OK", roleName: "push button", role: 43, states: showing, ifaces: []string{ifaceComponent, ifaceAction},
			actions: []string{"click"}, screen: image.Rect(330, 330, 400, 350)},
		"/closed": {name: "Old", roleName: "frame", role: 23, states: []int{stEnabled}},
	}
	return &fakeATSPI{objs: objs, apps: []atspiRef{{Name: fakeApp, Path: app}}, pidOf: 4242}
}

func fakeAccessorOver(bus *fakeATSPI, ctl Controller) *atspiAccessor {
	return &atspiAccessor{ctl: ctl, dial: func(context.Context) (atspiBus, error) { return bus, nil }}
}

func readTree(t *testing.T, a *atspiAccessor, target *Window) *Tree {
	t.Helper()
	tree, err := a.Read(context.Background(), target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	tree.finish(image.Rect(0, 0, 1920, 1080))
	return tree
}

func TestATSPIReadsTheActiveWindowAndItsDialog(t *testing.T) {
	bus := fixtureBus()
	m := newMachine("linux", "xdotool")
	tree := readTree(t, fakeAccessorOver(bus, NewController(m.Env())), nil)
	if tree.App != "editor" || tree.Window != "Notes" || tree.PID != 4242 || !tree.Complete || !tree.Positioned {
		t.Fatalf("tree = %+v", tree)
	}
	got := labels(tree)
	// The dialog's button comes first; the button under the dialog and the
	// one not showing are gone.
	want := `e1:button "OK" e2:textfield "Title" e3:checkbox "Wrap" e4:treeitem "Folders" e5:slider "Zoom" e6:button "Save"`
	if got != want {
		t.Fatalf("elements:\n got %s\nwant %s", got, want)
	}
	entry, _ := tree.Find("e2")
	if entry.Value != "draft" || !entry.Focused || entry.Can(ActPress) || !entry.Can(ActSetText) || !entry.Can(ActFocus) {
		t.Errorf("entry = %+v (an entry's activate is Enter, not a press)", entry)
	}
	check, _ := tree.Find("e3")
	if check.Checked != TriOn || !check.Can(ActPress) {
		t.Errorf("check = %+v", check)
	}
	folder, _ := tree.Find("e4")
	if folder.Expanded != TriOff || !folder.Can(ActExpand) || !folder.Can(ActCollapse) {
		t.Errorf("tree item = %+v", folder)
	}
	if zoom, _ := tree.Find("e5"); zoom.Value != "1.5" {
		t.Errorf("slider = %+v", zoom)
	}
	if save, _ := tree.Find("e6"); !save.Default || save.ID != "save-btn" || save.Bounds != image.Rect(450, 440, 550, 480) {
		t.Errorf("save = %+v", save)
	}
	for _, c := range bus.calls {
		if strings.HasPrefix(c, "/label Action.") || strings.HasPrefix(c, "/slider Action.") {
			t.Errorf("an interface the node lacks was called: %s", c)
		}
		if strings.HasPrefix(c, "/never") {
			t.Errorf("a hidden node's children were read: %s", c)
		}
	}
}

// A toolkit that cannot know where its window is reports window-relative
// rectangles under the screen coordinate type; the window manager's geometry
// puts them back on the screen.
func TestATSPICalibratesWindowRelativeToolkits(t *testing.T) {
	bus := fixtureBus()
	f := bus.objs["/frame"]
	f.screen, f.window = image.Rect(0, 0, 500, 400), image.Rect(0, 0, 500, 400)
	bus.objs["/save"].screen = image.Rect(350, 340, 450, 380)
	delete(bus.objs, "/dialog")
	bus.objs[atspiRootPath].children = []dbus.ObjectPath{"/frame"}
	m := newMachine("linux", "wmctrl")
	m.outputs["wmctrl -l -G -p -x"] = "0x0a000001  0 4242   100  100  500  400  editor.Editor  box  Notes\n"
	tree := readTree(t, fakeAccessorOver(bus, NewController(m.Env())), nil)
	if tree.WindowID != "0x0a000001" {
		t.Errorf("window id = %q", tree.WindowID)
	}
	for _, e := range tree.Elements {
		if e.Name == "Save" && e.Bounds != image.Rect(450, 440, 550, 480) {
			t.Errorf("Save at %v, want it moved onto the screen", e.Bounds)
		}
	}
}

func TestATSPIChoosesTheWindowAskedFor(t *testing.T) {
	bus := fixtureBus()
	delete(bus.objs, "/dialog")
	bus.objs[atspiRootPath].children = []dbus.ObjectPath{"/frame", "/other"}
	bus.objs["/frame"].states = showing
	bus.objs["/other"] = &fakeObj{name: "Other", roleName: "frame", role: 23, states: showing, ifaces: []string{ifaceComponent},
		screen: image.Rect(0, 0, 10, 10), children: []dbus.ObjectPath{"/save"}}
	a := fakeAccessorOver(bus, NewController(newMachine("linux").Env()))

	if tree := readTree(t, a, &Window{Title: "Other", PID: 4242}); tree.Window != "Other" {
		t.Errorf("read %q", tree.Window)
	}
	if tree := readTree(t, a, &Window{Title: "Notes"}); tree.Window != "Notes" {
		t.Errorf("read %q", tree.Window)
	}
	if _, err := a.Read(context.Background(), &Window{Title: "Elsewhere", PID: 1}); err == nil || !strings.Contains(err.Error(), "does not publish an accessibility tree") {
		t.Errorf("an unknown window: %v", err)
	}
	// Two windows and none active, no window manager to ask: nothing to pick.
	if _, err := a.Read(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "no window in front") {
		t.Errorf("ambiguous: %v", err)
	}
	// The window manager's answer settles it.
	m := newMachine("linux", "xdotool")
	m.outputs["xdotool getactivewindow"] = "1"
	m.outputs["xdotool getwindowname"] = "Other"
	m.outputs["xdotool getwindowpid"] = "4242"
	a.ctl = NewController(m.Env())
	if tree, err := a.Read(context.Background(), nil); err != nil || tree.Window != "Other" {
		t.Errorf("active by the window manager: %v %+v", err, tree)
	}
}

func TestATSPIWaylandHasNoPositions(t *testing.T) {
	bus := fixtureBus()
	m := newMachine("linux")
	m.env = map[string]string{"WAYLAND_DISPLAY": "wayland-0", "XDG_SESSION_TYPE": "wayland"}
	tree := readTree(t, fakeAccessorOver(bus, NewController(m.Env())), nil)
	if tree.Positioned {
		t.Fatal("a Wayland tree claims positions")
	}
	for _, e := range tree.Elements {
		if e.HasBounds() {
			t.Errorf("%s has bounds on Wayland: %v", e.Label(), e.Bounds)
		}
	}
	// Covering needs positions, so the button behind the dialog is listed.
	if !strings.Contains(labels(tree), `"Behind"`) {
		t.Errorf("unpositioned elements were dropped as covered: %s", labels(tree))
	}
}

func TestATSPIReadIsBoundedByItsDeadline(t *testing.T) {
	bus := fixtureBus()
	a := fakeAccessorOver(bus, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, _ := a.conn(ctx)
	tops, err := a.topLevels(ctx, b)
	if err != nil || len(tops) != 2 {
		t.Fatalf("top levels: %v %d", err, len(tops))
	}
	cancel()
	w := &walk{a: a, b: b, ctx: ctx, positioned: true}
	w.start(tops[0].node, 0, image.Point{})
	w.wg.Wait()
	if !w.cut {
		t.Error("a walk past its deadline says it is complete")
	}
}

func TestATSPIActsAndReadsBack(t *testing.T) {
	bus := fixtureBus()
	pressed := 0
	bus.objs["/check"].doAction = func(i int32) bool {
		pressed++
		bus.objs["/check"].states = append([]int{stFocusable}, showing...) // unchecked now
		return i == 0
	}
	a := fakeAccessorOver(bus, nil)
	tree := readTree(t, a, nil)
	ctx := context.Background()

	check, _ := tree.Find("e3")
	out, err := a.Do(ctx, check, Act{Kind: ActPress})
	if err != nil || out.After.Checked != TriOff || pressed != 1 {
		t.Fatalf("press: %v %+v pressed %d", err, out, pressed)
	}

	entry, _ := tree.Find("e2")
	out, err = a.Do(ctx, entry, Act{Kind: ActSetText, Text: "final"})
	if err != nil || out.After.Value != "final" {
		t.Fatalf("set text: %v %+v", err, out)
	}
	if out.After.Name != "Title" {
		t.Errorf("the label-derived name was lost: %+v", out.After)
	}
	if _, err := a.Do(ctx, entry, Act{Kind: ActFocus}); err != nil {
		t.Errorf("focus: %v", err)
	}
	if _, err := a.Do(ctx, entry, Act{Kind: ActPress}); !errors.Is(err, ErrRefused) {
		t.Errorf("pressing a text field: %v", err)
	}
	folder, _ := tree.Find("e4")
	if _, err := a.Do(ctx, folder, Act{Kind: ActExpand}); err != nil {
		t.Errorf("expand: %v", err)
	}
	if _, err := a.Do(ctx, folder, Act{Kind: "fly"}); !errors.Is(err, ErrRefused) {
		t.Errorf("unknown action: %v", err)
	}

	// The application says no: refused, nothing delivered.
	bus.objs["/save"].doAction = func(int32) bool { return false }
	save, _ := tree.Find("e6")
	if _, err := a.Do(ctx, save, Act{Kind: ActPress}); !errors.Is(err, ErrRefused) {
		t.Errorf("a refused press: %v", err)
	}
	bus.objs["/entry"].setText = func(string) bool { return false }
	if _, err := a.Do(ctx, entry, Act{Kind: ActSetText, Text: "x"}); !errors.Is(err, ErrRefused) {
		t.Errorf("a refused write: %v", err)
	}

	// Pressing OK closes the dialog: the button is gone, which is success.
	bus.objs["/ok"].doAction = func(int32) bool {
		delete(bus.objs, "/ok")
		return true
	}
	ok, _ := tree.Find("e1")
	if out, err := a.Do(ctx, ok, Act{Kind: ActPress}); err != nil || !out.Gone {
		t.Errorf("a button that closed its dialog: %v %+v", err, out)
	}
}

func TestATSPIRefusesWhatChanged(t *testing.T) {
	bus := fixtureBus()
	a := fakeAccessorOver(bus, nil)
	tree := readTree(t, a, nil)
	ctx := context.Background()
	save, _ := tree.Find("e6")

	bus.objs["/save"].name = "Discard"
	bus.objs["/save"].id = ""
	if _, err := a.Refresh(ctx, save); !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), `button "Discard"`) {
		t.Errorf("a renamed control: %v", err)
	}
	bus.objs["/save"].states = []int{stEnabled}
	if _, err := a.Do(ctx, save, Act{Kind: ActPress}); !errors.Is(err, ErrStale) {
		t.Errorf("a hidden control: %v", err)
	}
	delete(bus.objs, "/save")
	if _, err := a.Refresh(ctx, save); !errors.Is(err, ErrStale) {
		t.Errorf("a destroyed control: %v", err)
	}
	if _, err := a.Refresh(ctx, Element{Handle: "nonsense"}); !errors.Is(err, ErrStale) {
		t.Errorf("a foreign handle: %v", err)
	}
	// An error that is about the bus, not the object, is not staleness.
	bus.failAll = errors.New("timeout")
	if _, err := a.Refresh(ctx, save); err == nil || errors.Is(err, ErrStale) {
		t.Errorf("a timeout read as stale: %v", err)
	}
}

// A connection that dropped is dialled again once, so a session that
// restarted its accessibility bus does not need Factor restarted too.
func TestATSPIRedialsALostConnection(t *testing.T) {
	first := fixtureBus()
	first.failAll = dbus.ErrClosed
	second := fixtureBus()
	dials := 0
	a := &atspiAccessor{dial: func(context.Context) (atspiBus, error) {
		dials++
		if dials == 1 {
			return first, nil
		}
		return second, nil
	}}
	if _, err := a.Read(context.Background(), nil); err != nil {
		t.Fatalf("read after a lost connection: %v", err)
	}
	if dials != 2 || !first.closed {
		t.Errorf("dials %d, first closed %t", dials, first.closed)
	}
	failing := &atspiAccessor{dial: func(context.Context) (atspiBus, error) { return nil, errors.New("no bus") }}
	if _, err := failing.Read(context.Background(), nil); err == nil {
		t.Error("a missing bus read as a tree")
	}
	if _, err := failing.Refresh(context.Background(), Element{Handle: ":1.1|/x|0|0"}); err == nil {
		t.Error("a missing bus refreshed")
	}
	if _, err := failing.Do(context.Background(), Element{Handle: ":1.1|/x|0|0"}, Act{Kind: ActPress}); err == nil {
		t.Error("a missing bus acted")
	}
}

func TestATSPIHelpers(t *testing.T) {
	for role, want := range map[string]string{
		"push button": RoleButton, "toggle button": RoleToggle, "check box": RoleCheckbox, "radio button": RoleRadio,
		"switch": RoleSwitch, "check menu item": RoleMenuItem, "menu": RoleMenu, "combo box": RoleCombobox,
		"entry": RoleTextField, "password text": RolePassword, "spin button": RoleSpin, "link": RoleLink,
		"page tab": RoleTab, "list item": RoleListItem, "tree item": RoleTreeItem, "table cell": RoleCell,
		"icon": RoleIcon, "heading": RoleLabel, "filler": "",
	} {
		if got := canonicalRole(role); got != want {
			t.Errorf("canonicalRole(%q) = %q, want %q", role, got, want)
		}
	}
	if pressAction(RoleButton, []string{"showcontextmenu", "press"}) != 1 || pressAction(RoleButton, []string{"menu"}) != -1 {
		t.Error("pressAction")
	}
	if expandAction([]string{"click", "expand"}, true) != 1 || expandAction([]string{"collapse"}, false) != 0 || expandAction([]string{"x"}, true) != -1 {
		t.Error("expandAction")
	}
	n := &atspiNode{role: "toggle button", states: [2]uint32{1 << stPressed}}
	if e := n.element(0, image.Point{}, true); e.Checked != TriOn || e.Role != RoleToggle {
		t.Errorf("a pressed toggle: %+v", e)
	}
	if n.has(99) {
		t.Error("an out-of-range state bit")
	}
	for addr, env := range map[string]map[string]string{
		"unix:path=/x/bus":          {"DBUS_SESSION_BUS_ADDRESS": "unix:path=/x/bus"},
		"unix:path=/run/user/7/bus": {"XDG_RUNTIME_DIR": "/run/user/7"},
	} {
		if got := sessionBusAddress(Env{Getenv: func(k string) string { return env[k] }}); got != addr {
			t.Errorf("session bus = %q, want %q", got, addr)
		}
	}
	if !strings.HasPrefix(sessionBusAddress(Env{}), "unix:path=/run/user/") {
		t.Error("the fallback session bus is not in /run/user")
	}
	if goneObject(errors.New("plain")) || !goneObject(dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}) {
		t.Error("goneObject")
	}
	// Dialling a bus that is not there fails fast and says why.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dialATSPI(ctx, Env{Getenv: func(k string) string {
		return map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/nonexistent/bus"}[k]
	}}); err == nil {
		t.Error("dialled a bus that does not exist")
	}
	if _, err := dialATSPI(ctx, Env{Getenv: func(k string) string {
		return map[string]string{"AT_SPI_BUS_ADDRESS": "unix:path=/nonexistent/a11y"}[k]
	}}); err == nil {
		t.Error("connected to an accessibility bus that does not exist")
	}
	if NewAccessor(Env{GOOS: "linux"}, nil).Name() != "atspi" {
		t.Error("linux accessor")
	}
}

// A frozen application on the bus costs the read a moment, not the tree of
// the window in front.
func TestATSPISurvivesAHungApplication(t *testing.T) {
	bus := fixtureBus()
	bus.apps = append([]atspiRef{{Name: ":1.66", Path: atspiRootPath}}, bus.apps...)
	bus.hang = map[string]bool{":1.66": true}
	ctx, cancel := context.WithTimeout(context.Background(), readDeadline)
	defer cancel()
	started := time.Now()
	tree, err := fakeAccessorOver(bus, nil).Read(ctx, nil)
	if err != nil || tree.Window != "Notes" {
		t.Fatalf("read with a hung application: %v %+v", err, tree)
	}
	if took := time.Since(started); took > appProbe+500*time.Millisecond {
		t.Errorf("the hung application held the read for %v", took)
	}
}
