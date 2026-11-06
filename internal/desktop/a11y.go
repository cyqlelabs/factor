package desktop

// The accessibility layer: what the operating system's own accessibility
// tree says is on screen — AT-SPI on Linux, UI Automation on Windows, the AX
// API on macOS — laid over the grid rather than instead of it.
//
// The grid works on anything that draws pixels, and that is the guarantee it
// keeps; what it cannot do is say what anything is. The tree is the opposite
// bargain: it names a control, says whether a box is ticked or a button is
// greyed out, gives the exact rectangle it occupies, and can press it through
// the application itself without touching the pointer — but only for the
// applications that publish one, and only as well as their toolkit does. A
// game, a canvas-drawn web app, a remote desktop and a terminal's contents
// are pixels to it. So the two are read together and neither is trusted
// where it is blind: the tree tags what it knows on the grid's picture, the
// grid covers everything it does not, and a thin tree says it is thin so the
// model does not read a short list as a short screen.

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sort"
	"strings"
	"time"
)

// Tri is a state that only some controls have: a checkbox is checked or not,
// a button is neither.
type Tri int8

const (
	TriNone Tri = iota // the control has no such state
	TriOff
	TriOn
)

// triOf is the state of a control that has one.
func triOf(on bool) Tri {
	if on {
		return TriOn
	}
	return TriOff
}

// Canonical roles. Every backend maps its own taxonomy onto these and never
// invents one, so a listing reads the same on every platform.
const (
	RoleButton    = "button"
	RoleToggle    = "toggle"
	RoleCheckbox  = "checkbox"
	RoleRadio     = "radio"
	RoleSwitch    = "switch"
	RoleMenuItem  = "menuitem"
	RoleMenu      = "menu"
	RoleCombobox  = "combobox"
	RoleTextField = "textfield"
	RolePassword  = "password"
	RoleSpin      = "spinbutton"
	RoleSlider    = "slider"
	RoleLink      = "link"
	RoleTab       = "tab"
	RoleListItem  = "listitem"
	RoleTreeItem  = "treeitem"
	RoleCell      = "cell"
	RoleIcon      = "icon"
	// RoleLabel is static text: never listed, but what an unnamed field
	// beside it is called.
	RoleLabel = "label"
	// RoleOther is a container that advertised an action of its own.
	RoleOther = "other"
)

// Semantic actions: what a backend can do to a control through the
// application's own interface, with no pointer and no keystroke.
const (
	ActPress    = "press"    // the control's primary action: click a button, toggle a box, open a menu
	ActSetText  = "settext"  // replace a field's whole value
	ActFocus    = "focus"    // give it keyboard focus
	ActExpand   = "expand"   // open a disclosure or a tree row
	ActCollapse = "collapse" // close one
)

// interactiveRoles are listed whether or not they advertised an action: a
// text field is something to type into even when its toolkit offers no
// action for it.
var interactiveRoles = map[string]bool{
	RoleButton: true, RoleToggle: true, RoleCheckbox: true, RoleRadio: true, RoleSwitch: true,
	RoleMenuItem: true, RoleMenu: true, RoleCombobox: true, RoleTextField: true, RolePassword: true,
	RoleSpin: true, RoleSlider: true, RoleLink: true, RoleTab: true, RoleTreeItem: true,
}

// actionRoles are listed only when they advertise something to do: a list
// item or a cell that cannot be selected or pressed is just text.
var actionRoles = map[string]bool{RoleListItem: true, RoleCell: true, RoleIcon: true, RoleOther: true}

// Element is one control as the tree reported it.
type Element struct {
	Ref      string // "e7": assigned per frame, never by a backend
	Role     string
	Name     string
	Value    string // what the control holds; always empty for a secret field
	ID       string // the strongest identifier the toolkit assigned, if any
	Bounds   image.Rectangle
	Focused  bool
	Disabled bool
	Selected bool
	ReadOnly bool
	Default  bool
	Checked  Tri
	Expanded Tri
	Actions  []string
	Secret   bool
	// Layer is 0 for the window itself and higher for a popup, menu or
	// dialog of the same application stacked above it.
	Layer int
	// Handle is the backend's own address for the element, which is how it
	// is found again before anything acts on it.
	Handle string
}

// Can reports whether the backend offered an action on this element.
func (e Element) Can(act string) bool {
	for _, a := range e.Actions {
		if a == act {
			return true
		}
	}
	return false
}

// Center is the middle of the element's rectangle.
func (e Element) Center() image.Point {
	return image.Point{X: (e.Bounds.Min.X + e.Bounds.Max.X) / 2, Y: (e.Bounds.Min.Y + e.Bounds.Max.Y) / 2}
}

// HasBounds reports whether the element has a usable position.
func (e Element) HasBounds() bool { return e.Bounds.Dx() > 0 && e.Bounds.Dy() > 0 }

// Label is how the element is named in prose: its role and its name.
func (e Element) Label() string {
	if e.Name == "" {
		return e.Role + " (unnamed)"
	}
	return fmt.Sprintf("%s %q", e.Role, e.Name)
}

// interactive reports whether the element is a control worth a ref.
func (e Element) interactive() bool {
	if interactiveRoles[e.Role] {
		return true
	}
	return actionRoles[e.Role] && (e.Can(ActPress) || e.Can(ActExpand) || e.Selected)
}

// states names what a listing says about the element beyond its name.
func (e Element) states() []string {
	var out []string
	if e.Focused {
		out = append(out, "focused")
	}
	switch e.Checked {
	case TriOn:
		out = append(out, "checked")
	case TriOff:
		out = append(out, "unchecked")
	}
	switch e.Expanded {
	case TriOn:
		out = append(out, "expanded")
	case TriOff:
		out = append(out, "collapsed")
	}
	if e.Selected {
		out = append(out, "selected")
	}
	if e.Default {
		out = append(out, "default")
	}
	if e.ReadOnly {
		out = append(out, "read-only")
	}
	if e.Disabled {
		out = append(out, "disabled")
	}
	return out
}

// Tree is one observation of one window.
type Tree struct {
	Backend string
	App     string
	Window  string
	PID     int
	// WindowID is the controller's id for the window the tree belongs to,
	// when one matched, so a pointer fallback can bring it to the front.
	WindowID string
	Elements []Element
	// Withheld counts controls left out of the listing for room.
	Withheld int
	// Nodes is how many nodes the backend read to get here.
	Nodes int
	// Complete is false when the read ran out of time before the whole
	// window was walked: what is listed is real, what is missing is unknown.
	Complete bool
	// Positioned is whether the rectangles are screen coordinates the
	// pointer can use. Wayland gives an application no way to know where its
	// window is, so there the tree names controls but cannot place them.
	Positioned bool
	// Layers holds the rectangle of every surface above the window, by
	// layer: an element under one of them is covered, whatever its own
	// state says.
	Layers []image.Rectangle
	Took   time.Duration
	// Note says why a tree is empty, when it is.
	Note string
	// Frame is the image the tree was read beside, set by the vision store.
	Frame int
}

// Errors a backend answers an action with. They decide what the tool does
// next, so they are distinct: a stale element is refused outright, while a
// refused action may still be done with the pointer.
var (
	// ErrStale is an element that is gone, or is no longer the element
	// that was listed: acting on whatever now sits at its address would be
	// acting on something the model never saw.
	ErrStale = errors.New("the element is no longer there")
	// ErrRefused is an action the application would not perform, or does
	// not offer for that element. Nothing was delivered.
	ErrRefused = errors.New("the application refused the action")
)

// Act is one semantic action to perform.
type Act struct {
	Kind string
	Text string // ActSetText: the new value
}

// Outcome is what an action left behind.
type Outcome struct {
	// After is the element as read once the action had landed.
	After Element
	// Gone means the element disappeared after its own action, which is
	// what pressing OK on a dialog does: the action worked and the control
	// went with the surface it closed.
	Gone bool
}

// Accessor reads and drives one platform's accessibility tree.
type Accessor interface {
	// Name is the backend's label for logs and traces.
	Name() string
	// Read observes one window — target, or the window in front when nil —
	// returning whatever it read by ctx's deadline.
	Read(ctx context.Context, target *Window) (*Tree, error)
	// Refresh finds an element again and reads it as it is now. It answers
	// ErrStale when the element is gone or is no longer the same control.
	Refresh(ctx context.Context, el Element) (Element, error)
	// Do performs act on el after finding it again, and reports what the
	// element looks like afterwards. ErrStale means nothing was done because
	// the element is gone; ErrRefused means the application declined and
	// nothing was delivered; any other error may have been delivered.
	Do(ctx context.Context, el Element, act Act) (Outcome, error)
}

// NewAccessor returns the accessibility backend for the platform, or nil
// where there is none to read.
func NewAccessor(env Env, ctl Controller) Accessor {
	switch env.GOOS {
	case "windows":
		return newUIA(env)
	case "darwin":
		return newAX(env)
	}
	return newPlatformAccessor(env, ctl)
}

const (
	// maxListed bounds the controls one look lists. Past it a listing stops
	// helping a model pick one and starts costing every later turn.
	maxListed = 60
	// readDeadline bounds one tree read. It runs beside the screenshot, so
	// within it the read costs no time at all; measured, a 200-node GTK
	// window answers in about 120 ms with calls in flight.
	readDeadline = 1500 * time.Millisecond
	// thinNamed is the fewest named controls a tree needs to be worth
	// leaning on; below it the application draws most of itself.
	thinNamed = 3
	// treeMaxNodes bounds one read: a window past this is a document, and
	// the controls a model can use are long since listed.
	treeMaxNodes = 1500
	// treeMaxChildren bounds how many children of one node are read. A list
	// that manages its own descendants can claim a hundred thousand.
	treeMaxChildren = 200
)

// budgeted is a backend whose reads need longer than readDeadline: one that
// starts an interpreter before it can walk anything.
type budgeted interface{ Budget() time.Duration }

// readBudget is how long a read on this backend is given.
func readBudget(a Accessor) time.Duration {
	if b, ok := a.(budgeted); ok {
		return b.Budget()
	}
	return readDeadline
}

// Tree verdicts.
const (
	QualityRich = "rich"
	QualityThin = "thin"
	QualityNone = "none"
)

// Quality says how much of the window the tree can be trusted to describe.
func (t *Tree) Quality() string {
	if t == nil || len(t.Elements) == 0 {
		return QualityNone
	}
	named := t.named()
	if named < thinNamed || named*2 < len(t.Elements) {
		return QualityThin
	}
	return QualityRich
}

func (t *Tree) named() int {
	n := 0
	for _, e := range t.Elements {
		if e.Name != "" {
			n++
		}
	}
	return n
}

// Find returns the listed element with a ref.
func (t *Tree) Find(ref string) (Element, bool) {
	if t == nil {
		return Element{}, false
	}
	for _, e := range t.Elements {
		if e.Ref == ref {
			return e, true
		}
	}
	return Element{}, false
}

// finish turns what a backend read into what a model is shown: unnamed
// fields named after the label beside them, everything that is not a control
// dropped, controls covered by a popup of the same application dropped, the
// rest in reading order under a cap that always keeps the focused control,
// and refs assigned in that order.
func (t *Tree) finish(screen image.Rectangle) {
	nameFromLabels(t.Elements)
	var kept []Element
	for _, e := range t.Elements {
		if !e.interactive() {
			continue
		}
		if t.Positioned && t.covered(e) {
			continue
		}
		if t.Positioned && e.HasBounds() && !screen.Empty() && !e.Bounds.Overlaps(screen) {
			continue
		}
		if e.Secret {
			e.Value = ""
		}
		kept = append(kept, e)
	}
	sortReading(kept)
	if len(kept) > maxListed {
		kept, t.Withheld = capKeepingFocus(kept, maxListed)
	}
	for i := range kept {
		kept[i].Ref = fmt.Sprintf("e%d", i+1)
	}
	t.Elements = kept
}

// covered reports an element whose center lies under a surface stacked above
// its own: a dialog over the window, a menu over the dialog.
func (t *Tree) covered(e Element) bool {
	if !e.HasBounds() {
		return false
	}
	c := e.Center()
	for layer := e.Layer + 1; layer < len(t.Layers); layer++ {
		if c.In(t.Layers[layer]) {
			return true
		}
	}
	return false
}

// sortReading orders controls the way a person reads the screen: surfaces
// above first, then top to bottom in rows, left to right within one. A
// control with no position keeps the backend's order after the rest.
func sortReading(els []Element) {
	const row = 12 // px: controls this close vertically read as one row
	sort.SliceStable(els, func(i, j int) bool {
		a, b := els[i], els[j]
		if a.Layer != b.Layer {
			return a.Layer > b.Layer
		}
		if a.HasBounds() != b.HasBounds() {
			return a.HasBounds()
		}
		if !a.HasBounds() {
			return false
		}
		if ra, rb := a.Bounds.Min.Y/row, b.Bounds.Min.Y/row; ra != rb {
			return ra < rb
		}
		return a.Bounds.Min.X < b.Bounds.Min.X
	})
}

// capKeepingFocus keeps the first n controls, swapping the focused one in for
// the last when it would otherwise be dropped: the control with focus is the
// one keystrokes land in, and a listing without it hides where they go.
func capKeepingFocus(els []Element, n int) ([]Element, int) {
	kept := append([]Element(nil), els[:n]...)
	for _, e := range els[n:] {
		if e.Focused {
			kept[n-1] = e
			break
		}
	}
	return kept, len(els) - n
}

// nameFromLabels gives an unnamed field the text of the label beside it. A
// toolkit that never wired the label to its field still draws the two side by
// side, and the person reading the form reads them as one: a label just to
// the left on the same row, or just above and overlapping it horizontally.
func nameFromLabels(els []Element) {
	var labels []Element
	for _, e := range els {
		if e.Role == RoleLabel && e.Name != "" && e.HasBounds() {
			labels = append(labels, e)
		}
	}
	for i := range els {
		e := &els[i]
		if e.Name != "" || !e.HasBounds() || !namedByLabel(e.Role) {
			continue
		}
		best, bestGap := "", 1<<30
		for _, l := range labels {
			if l.Layer != e.Layer {
				continue
			}
			if gap, ok := labelGap(l.Bounds, e.Bounds); ok && gap < bestGap {
				best, bestGap = l.Name, gap
			}
		}
		e.Name = strings.TrimSuffix(strings.TrimSpace(best), ":")
	}
}

// namedByLabel is the roles a separate label names: fields, not buttons,
// which carry their own text.
func namedByLabel(role string) bool {
	switch role {
	case RoleTextField, RolePassword, RoleSpin, RoleSlider, RoleCombobox:
		return true
	}
	return false
}

// labelGap measures how far a label sits from a field it could name, and
// whether it is placed to name it at all.
func labelGap(label, field image.Rectangle) (int, bool) {
	const (
		maxLeft   = 300
		maxAbove  = 40
		maxIndent = 24
	)
	// Same row, ending at most maxLeft to the left of the field.
	lc := (label.Min.Y + label.Max.Y) / 2
	if lc >= field.Min.Y && lc <= field.Max.Y && label.Max.X <= field.Min.X+2 {
		if gap := field.Min.X - label.Max.X; gap <= maxLeft {
			return gap, true
		}
	}
	// Directly above and left-aligned with it, the way a form stacks a
	// label over its field. Overlapping is not enough: a status line above
	// a text area overlaps it too, and names nothing.
	if label.Max.Y <= field.Min.Y+2 && label.Min.X >= field.Min.X-maxIndent && label.Min.X <= field.Min.X+maxIndent {
		if gap := field.Min.Y - label.Max.Y; gap <= maxAbove {
			return gap, true
		}
	}
	return 0, false
}

// sameElement decides whether a fresh read is still the element that was
// listed. Role and the toolkit's own identifier must agree, and so must the
// name, which is a stable label for everything a ref is given to. Position,
// size and value are never identity: a moved button is the same button, and a
// field the user typed into is the same field.
func sameElement(was, now Element) bool {
	if was.Role != now.Role {
		return false
	}
	if was.ID != "" && now.ID != "" {
		return was.ID == now.ID
	}
	return was.Name == now.Name || ((was.Name == "" || now.Name == "") && namedByLabel(was.Role))
}

// clipValue bounds what a listing quotes of a control's contents.
func clipValue(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 60
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
