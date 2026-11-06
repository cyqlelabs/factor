//go:build linux

package desktop

// The Linux accessibility backend: AT-SPI, spoken over D-Bus in pure Go.
//
// AT-SPI is not a library an application links against for the reader's
// sake; it is a bus. Every GTK, Qt, Chromium and Firefox application that has
// accessibility on publishes its widgets as D-Bus objects on a second bus the
// session runs beside the ordinary one, and anything that can speak D-Bus can
// read them. So this needs no cgo, no libatspi and no helper program — which
// is what lets it ship in the same static binary as everything else.
//
// The read is shaped by what agent-desktop measured on the same stack
// (at-spi2-core 2.52, GNOME 46): about nine calls per node, 0.2–0.3 ms each,
// so a 200-node window costs ~430 ms one call at a time and ~120 ms with
// calls in flight. Interfaces are asked for before anything is called on
// them, because calling an interface a node does not implement makes the
// application print a CRITICAL warning for every node it happens on.

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	atspiRegistry = "org.a11y.atspi.Registry"
	atspiRootPath = dbus.ObjectPath("/org/a11y/atspi/accessible/root")

	ifaceAccessible = "org.a11y.atspi.Accessible"
	ifaceComponent  = "org.a11y.atspi.Component"
	ifaceAction     = "org.a11y.atspi.Action"
	ifaceText       = "org.a11y.atspi.Text"
	ifaceEditable   = "org.a11y.atspi.EditableText"
	ifaceValue      = "org.a11y.atspi.Value"
	ifaceProps      = "org.freedesktop.DBus.Properties"

	// coordScreen and coordWindow are AtspiCoordType.
	coordScreen = uint32(0)
	coordWindow = uint32(1)

	// relationLabelledBy is ATSPI_RELATION_LABELLED_BY.
	relationLabelledBy = uint32(2)

	// atspiInFlight is how many calls are outstanding at once. Measured,
	// 16 is within 5-15% of 128 and asks far less of the application.
	atspiInFlight = 16
	// atspiTextChars is how much of a field's contents is read.
	atspiTextChars = 256
)

// AtspiStateType bits that matter here.
const (
	stActive     = 1
	stChecked    = 4
	stDefunct    = 6
	stEditable   = 7
	stEnabled    = 8
	stExpandable = 9
	stExpanded   = 10
	stFocusable  = 11
	stFocused    = 12
	stModal      = 16
	stPressed    = 20
	stSelected   = 23
	stSensitive  = 24
	stShowing    = 25
	stVisible    = 30
	stDefault    = 39
	stCheckable  = 41
	stReadOnly   = 43
)

// atspiRef is how AT-SPI names an object: the bus name of the application
// that owns it and its path there.
type atspiRef struct {
	Name string
	Path dbus.ObjectPath
}

// atspiBus is the seam over the accessibility bus connection, narrow enough
// for a test to answer it from a scripted tree.
type atspiBus interface {
	// call invokes method on the object and stores the reply in out.
	call(ctx context.Context, ref atspiRef, method string, args []any, out ...any) error
	// pid is the process that owns a bus name.
	pid(ctx context.Context, busName string) (int, error)
	close()
}

// dbusBus is atspiBus over a real connection.
type dbusBus struct{ conn *dbus.Conn }

func (b *dbusBus) call(ctx context.Context, ref atspiRef, method string, args []any, out ...any) error {
	c := b.conn.Object(ref.Name, ref.Path).CallWithContext(ctx, method, 0, args...)
	if c.Err != nil {
		return c.Err
	}
	if len(out) == 0 {
		return nil
	}
	return c.Store(out...)
}

func (b *dbusBus) pid(ctx context.Context, busName string) (int, error) {
	var pid uint32
	err := b.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionUnixProcessID", 0, busName).Store(&pid)
	return int(pid), err
}

func (b *dbusBus) close() { _ = b.conn.Close() }

// dialATSPI finds the accessibility bus the way every AT-SPI client does —
// ask org.a11y.Bus on the session bus for its address, which also starts it
// on a session that has not used accessibility yet — and connects to it.
func dialATSPI(ctx context.Context, env Env) (atspiBus, error) {
	addr := env.env("AT_SPI_BUS_ADDRESS")
	if addr == "" {
		sess, err := dbus.Connect(sessionBusAddress(env), dbus.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("no session bus to find the accessibility bus on: %w", err)
		}
		defer func() { _ = sess.Close() }()
		if err := sess.Object("org.a11y.Bus", "/org/a11y/bus").CallWithContext(ctx, "org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil {
			return nil, fmt.Errorf("the accessibility bus did not answer (is at-spi2-core installed?): %w", err)
		}
	}
	// No WithContext here: that ties the connection's whole life to ctx,
	// and the context a read runs under ends with the read. The connection
	// is cached across calls; each call carries its own deadline.
	conn, err := dbus.Connect(addr)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to the accessibility bus: %w", err)
	}
	return &dbusBus{conn: conn}, nil
}

// sessionBusAddress is the session bus a process with no address of its own
// would use. A gateway started by a login entry or a system service carries
// none, while the user's session bus sits at the runtime directory the whole
// time — the same gap AdoptDisplay closes for the screen.
func sessionBusAddress(env Env) string {
	if a := env.env("DBUS_SESSION_BUS_ADDRESS"); a != "" {
		return a
	}
	if dir := env.env("XDG_RUNTIME_DIR"); dir != "" {
		return "unix:path=" + dir + "/bus"
	}
	return fmt.Sprintf("unix:path=/run/user/%d/bus", os.Getuid())
}

// newPlatformAccessor is the Linux accessor.
func newPlatformAccessor(env Env, ctl Controller) Accessor {
	return &atspiAccessor{env: env, ctl: ctl, dial: func(ctx context.Context) (atspiBus, error) { return dialATSPI(ctx, env) }}
}

// atspiAccessor reads and drives AT-SPI.
type atspiAccessor struct {
	env  Env
	ctl  Controller
	dial func(ctx context.Context) (atspiBus, error)

	mu    sync.Mutex
	bus   atspiBus
	roles map[uint32]string // role number → AT-SPI role name, asked once each
}

func (a *atspiAccessor) Name() string { return "atspi" }

// conn returns the cached connection, dialling one when there is none.
func (a *atspiAccessor) conn(ctx context.Context) (atspiBus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bus != nil {
		return a.bus, nil
	}
	b, err := a.dial(ctx)
	if err != nil {
		return nil, err
	}
	a.bus = b
	return b, nil
}

// drop forgets a connection that failed at the connection level, so the next
// read dials again: a session that restarted its accessibility bus is
// otherwise unreadable until Factor restarts.
func (a *atspiAccessor) drop(b atspiBus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bus == b {
		a.bus.close()
		a.bus = nil
	}
}

// connectionLost reports an error the connection itself caused, as opposed
// to one object answering badly.
func connectionLost(err error) bool {
	return errors.Is(err, dbus.ErrClosed) || err != nil && strings.Contains(err.Error(), "use of closed network connection")
}

// atspiRect is the (iiii) a Component answers GetExtents with.
type atspiRect struct{ X, Y, W, H int32 }

func (r atspiRect) rect() image.Rectangle {
	return image.Rect(int(r.X), int(r.Y), int(r.X+r.W), int(r.Y+r.H))
}

// atspiNode is one node as read.
type atspiNode struct {
	ref        atspiRef
	roleNum    uint32
	role       string // AT-SPI's own role name
	name       string
	desc       string
	id         string
	states     [2]uint32
	ifaces     map[string]bool
	screen     image.Rectangle
	window     image.Rectangle
	actions    []string
	text       string
	childCount int
}

func (n *atspiNode) has(bit int) bool {
	if bit < 0 || bit >= 64 {
		return false
	}
	return n.states[bit/32]&(1<<(bit%32)) != 0
}

func (n *atspiNode) showing() bool { return n.has(stShowing) && n.has(stVisible) }

// detail says how much of a node to read: a container needs only enough to
// decide whether to descend, a control needs everything a listing says.
type detail int

const (
	detailWalk detail = iota // role, states, name, children
	detailFull               // and extents, actions, contents
)

// readNode reads one node. Calls on interfaces the node lacks are never made.
func (a *atspiAccessor) readNode(ctx context.Context, b atspiBus, ref atspiRef, want detail) (*atspiNode, error) {
	n := &atspiNode{ref: ref, ifaces: map[string]bool{}}
	var props map[string]dbus.Variant
	if err := b.call(ctx, ref, ifaceProps+".GetAll", []any{ifaceAccessible}, &props); err != nil {
		return nil, err
	}
	n.name, _ = props["Name"].Value().(string)
	n.desc, _ = props["Description"].Value().(string)
	n.id, _ = props["AccessibleId"].Value().(string)
	if c, ok := props["ChildCount"].Value().(int32); ok {
		n.childCount = int(c)
	}
	if err := b.call(ctx, ref, ifaceAccessible+".GetRole", nil, &n.roleNum); err != nil {
		return nil, err
	}
	role, err := a.roleName(ctx, b, ref, n.roleNum)
	if err != nil {
		return nil, err
	}
	n.role = role
	var states []uint32
	if err := b.call(ctx, ref, ifaceAccessible+".GetState", nil, &states); err != nil {
		return nil, err
	}
	copy(n.states[:], states)
	// A container needs no more than that to be descended through, and in a
	// GTK4 window containers are most of the nodes.
	if want == detailWalk && !worthDetail(canonicalRole(n.role)) {
		return n, nil
	}
	var ifaces []string
	if err := b.call(ctx, ref, ifaceAccessible+".GetInterfaces", nil, &ifaces); err != nil {
		return nil, err
	}
	for _, i := range ifaces {
		n.ifaces[i] = true
	}
	if n.ifaces[ifaceComponent] {
		// The reply is one (iiii) struct, not four values.
		var r atspiRect
		if err := b.call(ctx, ref, ifaceComponent+".GetExtents", []any{coordScreen}, &r); err == nil {
			n.screen = r.rect()
		}
		if err := b.call(ctx, ref, ifaceComponent+".GetExtents", []any{coordWindow}, &r); err == nil {
			n.window = r.rect()
		}
	}
	if n.ifaces[ifaceAction] {
		var acts []struct{ Name, Description, Keys string }
		if err := b.call(ctx, ref, ifaceAction+".GetActions", nil, &acts); err == nil {
			for _, act := range acts {
				n.actions = append(n.actions, strings.ToLower(act.Name))
			}
		}
	}
	canon := canonicalRole(n.role)
	switch {
	case canon == RolePassword:
		// Never read: the contents of a secret field are not the tree's to
		// report, whatever the toolkit would hand over.
	case n.ifaces[ifaceText] && (canon == RoleTextField || canon == RoleSpin || canon == RoleCombobox || canon == RoleLabel && n.name == ""):
		var s string
		if err := b.call(ctx, ref, ifaceText+".GetText", []any{int32(0), int32(atspiTextChars)}, &s); err == nil {
			n.text = s
		}
	case n.ifaces[ifaceValue] && canon == RoleSlider:
		var v dbus.Variant
		if err := b.call(ctx, ref, ifaceProps+".Get", []any{ifaceValue, "CurrentValue"}, &v); err == nil {
			if f, ok := v.Value().(float64); ok {
				n.text = strconv.FormatFloat(f, 'f', -1, 64)
			}
		}
	}
	if n.name == "" && namedByLabel(canon) {
		n.name = a.labelledBy(ctx, b, ref)
	}
	return n, nil
}

// worthDetail is the roles whose extents and actions are read during a walk:
// everything that may be listed, and the labels that name fields beside them.
func worthDetail(role string) bool {
	return role != ""
}

// labelledBy reads the name of the label a field declares as its own.
func (a *atspiAccessor) labelledBy(ctx context.Context, b atspiBus, ref atspiRef) string {
	var rels []struct {
		Type    uint32
		Targets []atspiRef
	}
	if err := b.call(ctx, ref, ifaceAccessible+".GetRelationSet", nil, &rels); err != nil {
		return ""
	}
	for _, r := range rels {
		if r.Type != relationLabelledBy {
			continue
		}
		for _, target := range r.Targets {
			var v dbus.Variant
			if err := b.call(ctx, target, ifaceProps+".Get", []any{ifaceAccessible, "Name"}, &v); err == nil {
				if s, _ := v.Value().(string); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// roleName asks for a role number's name once and remembers it: the names
// are AT-SPI's own enum, the same for every application.
func (a *atspiAccessor) roleName(ctx context.Context, b atspiBus, ref atspiRef, num uint32) (string, error) {
	a.mu.Lock()
	name, ok := a.roles[num]
	a.mu.Unlock()
	if ok {
		return name, nil
	}
	if err := b.call(ctx, ref, ifaceAccessible+".GetRoleName", nil, &name); err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.roles == nil {
		a.roles = map[uint32]string{}
	}
	a.roles[num] = name
	a.mu.Unlock()
	return name, nil
}

// children lists a node's children, bounded.
func (a *atspiAccessor) children(ctx context.Context, b atspiBus, ref atspiRef) ([]atspiRef, error) {
	var kids []atspiRef
	if err := b.call(ctx, ref, ifaceAccessible+".GetChildren", nil, &kids); err != nil {
		return nil, err
	}
	if len(kids) > treeMaxChildren {
		kids = kids[:treeMaxChildren]
	}
	return kids, nil
}

// canonicalRole maps AT-SPI's role names onto the shared vocabulary. A role
// that is neither a control nor a label maps to "", which the walk descends
// through without listing.
func canonicalRole(role string) string {
	switch role {
	case "push button", "button", "push button menu":
		return RoleButton
	case "toggle button":
		return RoleToggle
	case "check box":
		return RoleCheckbox
	case "radio button":
		return RoleRadio
	case "switch":
		return RoleSwitch
	case "menu item", "check menu item", "radio menu item", "tearoff menu item":
		return RoleMenuItem
	case "menu":
		return RoleMenu
	case "combo box":
		return RoleCombobox
	case "text", "entry", "text box", "search box", "editbar":
		return RoleTextField
	case "password text":
		return RolePassword
	case "spin button":
		return RoleSpin
	case "slider":
		return RoleSlider
	case "link":
		return RoleLink
	case "page tab":
		return RoleTab
	case "list item":
		return RoleListItem
	case "tree item", "tree table cell":
		return RoleTreeItem
	case "table cell":
		return RoleCell
	case "icon":
		return RoleIcon
	case "label", "static", "heading", "caption", "paragraph":
		return RoleLabel
	}
	return ""
}

// pressNames are the action names that mean "do what a click does".
var pressNames = []string{"click", "press", "activate", "toggle", "jump", "open", "invoke", "push", "select"}

// pressAction finds a node's primary action, or -1. A text field's
// "activate" is Enter — the action that submits the form — and is never what
// pressing the field means.
func pressAction(role string, actions []string) int {
	if role == RoleTextField || role == RolePassword || role == RoleSpin {
		return -1
	}
	for _, want := range pressNames {
		for i, a := range actions {
			if a == want {
				return i
			}
		}
	}
	return -1
}

// expandAction finds the action that opens or closes a disclosure.
func expandAction(actions []string, open bool) int {
	for i, a := range actions {
		switch {
		case a == "expand or contract", a == "expand or collapse":
			return i
		case open && a == "expand", !open && (a == "collapse" || a == "contract"):
			return i
		}
	}
	return -1
}

// element turns a node into the shared shape. offset moves a toolkit's
// window-relative rectangles onto the screen where it could not do so itself.
func (n *atspiNode) element(layer int, offset image.Point, positioned bool) Element {
	role := canonicalRole(n.role)
	if role == "" && pressAction("", n.actions) >= 0 {
		role = RoleOther
	}
	e := Element{
		Role:     role,
		Name:     strings.TrimSpace(n.name),
		ID:       n.id,
		Focused:  n.has(stFocused),
		Disabled: !n.has(stEnabled) && !n.has(stSensitive),
		Selected: n.has(stSelected),
		Default:  n.has(stDefault),
		Secret:   role == RolePassword,
		Layer:    layer,
		Handle:   fmt.Sprintf("%s|%s|%d|%d", n.ref.Name, n.ref.Path, offset.X, offset.Y),
	}
	if role == RoleLabel && e.Name == "" {
		e.Name = strings.TrimSpace(n.text)
	} else if !e.Secret {
		e.Value = n.text
	}
	if positioned && !n.screen.Empty() {
		e.Bounds = n.screen.Add(offset)
	}
	switch {
	case role == RoleCheckbox || role == RoleRadio || role == RoleSwitch || n.has(stCheckable):
		e.Checked = triOf(n.has(stChecked) || n.has(stPressed))
	case role == RoleToggle:
		e.Checked = triOf(n.has(stPressed) || n.has(stChecked))
	}
	if n.has(stExpandable) {
		e.Expanded = triOf(n.has(stExpanded))
	}
	if role == RoleTextField || role == RoleSpin {
		e.ReadOnly = n.has(stReadOnly) || !n.has(stEditable)
	} else {
		e.ReadOnly = n.has(stReadOnly)
	}
	if pressAction(role, n.actions) >= 0 {
		e.Actions = append(e.Actions, ActPress)
	}
	if n.ifaces[ifaceEditable] && !e.ReadOnly {
		e.Actions = append(e.Actions, ActSetText)
	}
	if n.ifaces[ifaceComponent] && n.has(stFocusable) {
		e.Actions = append(e.Actions, ActFocus)
	}
	if n.has(stExpandable) {
		if expandAction(n.actions, true) >= 0 {
			e.Actions = append(e.Actions, ActExpand)
		}
		if expandAction(n.actions, false) >= 0 {
			e.Actions = append(e.Actions, ActCollapse)
		}
	}
	return e
}

// topLevel is one surface an application has open: its main window, a
// dialog, a menu.
type topLevel struct {
	app     atspiRef
	appName string
	pid     int
	node    *atspiNode
}

// surfaceRoles are the top-level roles that stack above a window rather than
// being windows of their own.
func surfaceRank(n *atspiNode) int {
	switch n.role {
	case "frame":
		return 0
	case "dialog", "file chooser", "alert", "color chooser", "font chooser":
		if n.has(stModal) {
			return 2
		}
		return 1
	case "window", "menu", "popup menu", "tool tip":
		return 3
	}
	return -1
}

// withBus runs fn on the cached connection, and once more on a fresh one if
// the first failed at the connection level.
func (a *atspiAccessor) withBus(ctx context.Context, fn func(b atspiBus) error) error {
	b, err := a.conn(ctx)
	if err != nil {
		return err
	}
	err = fn(b)
	if err != nil && connectionLost(err) {
		a.drop(b)
		if b, err = a.conn(ctx); err != nil {
			return err
		}
		err = fn(b)
	}
	return err
}

// Read observes one window and the surfaces its application has open above
// it.
func (a *atspiAccessor) Read(ctx context.Context, target *Window) (*Tree, error) {
	var tree *Tree
	err := a.withBus(ctx, func(b atspiBus) error {
		var err error
		tree, err = a.read(ctx, b, target)
		return err
	})
	return tree, err
}

func (a *atspiAccessor) read(ctx context.Context, b atspiBus, target *Window) (*Tree, error) {
	tops, err := a.topLevels(ctx, b)
	if err != nil {
		return nil, err
	}
	main, err := a.choose(ctx, tops, target)
	if err != nil {
		return nil, err
	}
	positioned := a.ctl == nil || a.ctl.Backend() != "wayland"
	tree := &Tree{
		Backend: a.Name(), App: main.appName, Window: main.node.name, PID: main.pid,
		Positioned: positioned, Complete: true,
	}
	wins := a.windows(ctx)
	if w, ok := matchWindow(wins, main); ok {
		tree.WindowID = w.ID
	}
	// The main window is layer 0; the application's dialogs and popups that
	// are open above it are stacked over it in rank order.
	layers := []topLevel{main}
	for _, rank := range []int{1, 2, 3} {
		for _, t := range tops {
			if t.app == main.app && t.node.ref != main.node.ref && surfaceRank(t.node) == rank {
				layers = append(layers, t)
			}
		}
	}
	w := &walk{a: a, b: b, ctx: ctx, positioned: positioned}
	for i, l := range layers {
		offset := calibrate(wins, l.node)
		tree.Layers = append(tree.Layers, l.node.screen.Add(offset))
		w.start(l.node, i, offset)
	}
	w.wg.Wait()
	tree.Elements = w.elements
	tree.Nodes = w.nodes
	tree.Complete = !w.cut
	sortByOrder(tree.Elements, w.order)
	return tree, nil
}

// topLevels lists every application's showing top-level surfaces.
func (a *atspiAccessor) topLevels(ctx context.Context, b atspiBus) ([]topLevel, error) {
	apps, err := a.children(ctx, b, atspiRef{Name: atspiRegistry, Path: atspiRootPath})
	if err != nil {
		return nil, fmt.Errorf("the accessibility registry did not answer: %w", err)
	}
	var out []topLevel
	for _, app := range apps {
		out = append(out, a.appSurfaces(ctx, b, app)...)
	}
	return out, nil
}

// appProbe bounds how long one application may take to say what it has
// open. Every application on the bus is asked, and a hung one — a frozen
// program is an ordinary thing on a desktop — would otherwise spend the
// whole read's deadline and leave no tree for the window in front.
const appProbe = 300 * time.Millisecond

// appSurfaces lists one application's showing top-level surfaces.
func (a *atspiAccessor) appSurfaces(ctx context.Context, b atspiBus, app atspiRef) []topLevel {
	ctx, cancel := context.WithTimeout(ctx, appProbe)
	defer cancel()
	var props map[string]dbus.Variant
	if err := b.call(ctx, app, ifaceProps+".GetAll", []any{ifaceAccessible}, &props); err != nil {
		return nil // gone between the two calls, or not answering
	}
	appName, _ := props["Name"].Value().(string)
	pid, _ := b.pid(ctx, app.Name)
	kids, err := a.children(ctx, b, app)
	if err != nil {
		return nil
	}
	var out []topLevel
	for _, k := range kids {
		n, err := a.readNode(ctx, b, k, detailFull)
		if err != nil || !n.showing() || surfaceRank(n) < 0 {
			continue
		}
		out = append(out, topLevel{app: app, appName: appName, pid: pid, node: n})
	}
	return out
}

// choose picks the window to read: the one asked for, the one the
// accessibility tree says is active, the one the window manager says is
// active, or the only one there is.
func (a *atspiAccessor) choose(ctx context.Context, tops []topLevel, target *Window) (topLevel, error) {
	frames := func(keep func(topLevel) bool) []topLevel {
		var out []topLevel
		for _, t := range tops {
			if surfaceRank(t.node) <= 1 && keep(t) {
				out = append(out, t)
			}
		}
		return out
	}
	if target == nil {
		if act := frames(func(t topLevel) bool { return t.node.has(stActive) }); len(act) > 0 {
			return act[0], nil
		}
		if a.ctl != nil {
			if w, err := a.ctl.ActiveWindow(ctx); err == nil {
				target = &w
			}
		}
	}
	if target != nil {
		for _, try := range []func(topLevel) bool{
			func(t topLevel) bool { return t.node.name == target.Title && (target.PID == 0 || t.pid == target.PID) },
			func(t topLevel) bool { return t.node.name == target.Title },
			func(t topLevel) bool { return target.PID != 0 && t.pid == target.PID },
		} {
			if m := frames(try); len(m) == 1 {
				return m[0], nil
			}
		}
		return topLevel{}, fmt.Errorf("%q does not publish an accessibility tree (it is not on the accessibility bus, or its toolkit has accessibility off)", target.Title)
	}
	if all := frames(func(topLevel) bool { return true }); len(all) == 1 {
		return all[0], nil
	}
	return topLevel{}, fmt.Errorf("no window in front publishes an accessibility tree")
}

// windows lists the window manager's windows once for a read, or nothing
// where there is no window manager to ask.
func (a *atspiAccessor) windows(ctx context.Context) []Window {
	if a.ctl == nil {
		return nil
	}
	wins, err := a.ctl.ListWindows(ctx)
	if err != nil {
		return nil
	}
	return wins
}

// matchWindow finds the controller's window for a tree's frame, by process
// and title, so a pointer fallback can bring it to the front first.
func matchWindow(wins []Window, t topLevel) (Window, bool) {
	var byTitle []Window
	for _, w := range wins {
		if w.Title != t.node.name {
			continue
		}
		if t.pid != 0 && w.PID == t.pid {
			return w, true
		}
		byTitle = append(byTitle, w)
	}
	if len(byTitle) == 1 {
		return byTitle[0], true
	}
	return Window{}, false
}

// calibrate finds the offset that puts a surface's rectangles on the screen.
// GTK3 and Chromium report screen coordinates of their own; a toolkit that
// cannot know where its window is (GTK4) reports window-relative ones under
// the screen coordinate type, which shows as a surface whose screen origin
// equals its window origin. The window manager does know, so its geometry
// supplies the origin.
func calibrate(wins []Window, n *atspiNode) image.Point {
	if n.screen.Empty() || n.screen.Min != n.window.Min {
		return image.Point{}
	}
	for _, w := range wins {
		if w.Title == n.name && w.HasGeom && (w.X != 0 || w.Y != 0) {
			return image.Point{X: w.X - n.screen.Min.X, Y: w.Y - n.screen.Min.Y}
		}
	}
	return image.Point{}
}

// walk reads a surface's subtree with calls in flight, under the read's
// deadline and node budget.
type walk struct {
	a          *atspiAccessor
	b          atspiBus
	ctx        context.Context
	positioned bool
	sem        chan struct{}
	wg         sync.WaitGroup

	mu       sync.Mutex
	elements []Element
	order    map[string]string // handle → document order key
	nodes    int
	cut      bool
}

// start reads a surface whose root has already been read.
func (w *walk) start(root *atspiNode, layer int, offset image.Point) {
	if w.sem == nil {
		w.sem = make(chan struct{}, atspiInFlight)
		w.order = map[string]string{}
	}
	w.visit(root, layer, offset, fmt.Sprintf("%02d", layer))
}

// visit records a node and walks its children.
func (w *walk) visit(n *atspiNode, layer int, offset image.Point, key string) {
	w.mu.Lock()
	w.nodes++
	if n.showing() {
		e := n.element(layer, offset, w.positioned)
		if e.Role != "" {
			w.elements = append(w.elements, e)
			w.order[e.Handle] = key
		}
	}
	w.mu.Unlock()
	// What is not showing is not descended into: a closed menu's items, a
	// hidden notebook page, a collapsed tree's rows.
	if !n.showing() || n.childCount == 0 || n.has(stDefunct) {
		return
	}
	kids, err := w.a.children(w.ctx, w.b, n.ref)
	if err != nil {
		w.mark()
		return
	}
	for i, k := range kids {
		if !w.budget() {
			return
		}
		w.wg.Add(1)
		go func(ref atspiRef, key string) {
			defer w.wg.Done()
			select {
			case w.sem <- struct{}{}:
			case <-w.ctx.Done():
				w.mark()
				return
			}
			child, err := w.a.readNode(w.ctx, w.b, ref, detailWalk)
			<-w.sem
			if err != nil {
				if w.ctx.Err() != nil {
					w.mark()
				}
				return
			}
			w.visit(child, layer, offset, key)
		}(k, fmt.Sprintf("%s.%04d", key, i))
	}
}

// budget reports whether another node may be read, marking the read cut
// when it may not.
func (w *walk) budget() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.nodes >= treeMaxNodes || w.ctx.Err() != nil {
		w.cut = true
		return false
	}
	return true
}

func (w *walk) mark() {
	w.mu.Lock()
	w.cut = true
	w.mu.Unlock()
}

// sortByOrder restores document order, which the concurrent walk scrambled;
// reading order is applied on top of it later, and controls with no
// position keep this one.
func sortByOrder(els []Element, order map[string]string) {
	sort.SliceStable(els, func(i, j int) bool { return order[els[i].Handle] < order[els[j].Handle] })
}

// parseHandle reads an element's address back.
func parseHandle(h string) (atspiRef, image.Point, error) {
	parts := strings.Split(h, "|")
	if len(parts) != 4 {
		return atspiRef{}, image.Point{}, fmt.Errorf("%w: not an AT-SPI element", ErrStale)
	}
	x, _ := strconv.Atoi(parts[2])
	y, _ := strconv.Atoi(parts[3])
	return atspiRef{Name: parts[0], Path: dbus.ObjectPath(parts[1])}, image.Point{X: x, Y: y}, nil
}

// goneObject reports an error that means the object is no longer there.
func goneObject(err error) bool {
	var de dbus.Error
	if errors.As(err, &de) {
		switch de.Name {
		case "org.freedesktop.DBus.Error.UnknownObject", "org.freedesktop.DBus.Error.UnknownMethod",
			"org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner",
			"org.freedesktop.DBus.Error.UnknownInterface":
			return true
		}
	}
	return false
}

// Refresh reads an element again and checks it is still the one listed.
func (a *atspiAccessor) Refresh(ctx context.Context, el Element) (Element, error) {
	ref, offset, err := parseHandle(el.Handle)
	if err != nil {
		return Element{}, err
	}
	var n *atspiNode
	err = a.withBus(ctx, func(b atspiBus) error {
		var err error
		n, err = a.readNode(ctx, b, ref, detailFull)
		return err
	})
	if err != nil {
		if goneObject(err) {
			return Element{}, ErrStale
		}
		return Element{}, err
	}
	if !n.showing() || n.has(stDefunct) {
		return Element{}, fmt.Errorf("%w: it is no longer showing", ErrStale)
	}
	positioned := a.ctl == nil || a.ctl.Backend() != "wayland"
	now := n.element(el.Layer, offset, positioned)
	if !sameElement(el, now) {
		return Element{}, fmt.Errorf("%w: its address now holds %s", ErrStale, now.Label())
	}
	now.Ref = el.Ref
	if now.Name == "" {
		now.Name = el.Name
	}
	return now, nil
}

// settleAfterAct is how long an action is given to land before the element
// is read back. GTK performs a click inside the call, but a state the click
// changes is applied on the application's next main-loop turn.
const settleAfterAct = 60 * time.Millisecond

// Do performs a semantic action and reads the element back.
func (a *atspiAccessor) Do(ctx context.Context, el Element, act Act) (Outcome, error) {
	fresh, err := a.Refresh(ctx, el)
	if err != nil {
		return Outcome{}, err
	}
	// The connection Refresh just used is the one to act on: a reconnect
	// between the two would be a fresh read of a different session.
	b, err := a.conn(ctx)
	if err != nil {
		return Outcome{}, err
	}
	ref, _, _ := parseHandle(fresh.Handle)
	var ok bool
	switch act.Kind {
	case ActPress, ActExpand, ActCollapse:
		var acts []struct{ Name, Description, Keys string }
		if err := b.call(ctx, ref, ifaceAction+".GetActions", nil, &acts); err != nil {
			return Outcome{}, fmt.Errorf("%w: %v", ErrRefused, err)
		}
		names := make([]string, len(acts))
		for i, x := range acts {
			names[i] = strings.ToLower(x.Name)
		}
		idx := pressAction(fresh.Role, names)
		if act.Kind != ActPress {
			idx = expandAction(names, act.Kind == ActExpand)
		}
		if idx < 0 {
			return Outcome{}, fmt.Errorf("%w: %s offers no %s action", ErrRefused, fresh.Label(), act.Kind)
		}
		err = b.call(ctx, ref, ifaceAction+".DoAction", []any{int32(idx)}, &ok)
	case ActSetText:
		if !fresh.Can(ActSetText) {
			return Outcome{}, fmt.Errorf("%w: %s cannot be written directly", ErrRefused, fresh.Label())
		}
		err = b.call(ctx, ref, ifaceEditable+".SetTextContents", []any{act.Text}, &ok)
	case ActFocus:
		err = b.call(ctx, ref, ifaceComponent+".GrabFocus", nil, &ok)
	default:
		return Outcome{}, fmt.Errorf("%w: unknown action %q", ErrRefused, act.Kind)
	}
	if err != nil {
		if goneObject(err) {
			return Outcome{}, ErrStale
		}
		// The call may have reached the application before it failed:
		// that is neither refused nor done, and the caller must say so.
		return Outcome{}, fmt.Errorf("the action may or may not have reached the application: %w", err)
	}
	if !ok {
		return Outcome{}, fmt.Errorf("%w: the application answered no", ErrRefused)
	}
	select {
	case <-time.After(settleAfterAct):
	case <-ctx.Done():
	}
	after, err := a.Refresh(ctx, fresh)
	if errors.Is(err, ErrStale) {
		// Pressing OK closes the dialog the button was in: an element that
		// is gone after its own action is the action having worked.
		return Outcome{After: fresh, Gone: true}, nil
	}
	if err != nil {
		return Outcome{After: fresh}, nil
	}
	return Outcome{After: after}, nil
}
