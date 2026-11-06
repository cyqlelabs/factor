package desktop

// The macOS accessibility backend: the AX API as System Events exposes it to
// JavaScript for Automation, run by the osascript every Mac has. It needs the
// same Accessibility permission the rest of the macOS controller already
// asks for, and nothing installed.
//
// Apple Events are the cost here, not the walk: every attribute read is one
// round trip to System Events. So attributes are read per container, all of
// its children at once — `kids.role()` answers for every child in a single
// event — which keeps a window to a few events per level instead of eight per
// control. An element is found again by its index path from the window and
// checked against the role and name it was listed with.

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"strconv"
	"strings"
	"time"
)

const axBudget = 3000 * time.Millisecond

type axAccessor struct{ env Env }

func newAX(env Env) Accessor { return &axAccessor{env: env} }

func (x *axAccessor) Name() string { return "ax" }

// Budget is the read deadline screen_view gives this backend.
func (x *axAccessor) Budget() time.Duration { return axBudget + 1000*time.Millisecond }

// jxa runs a JavaScript for Automation script, handed over on stdin.
func (x *axAccessor) jxa(ctx context.Context, script string) (string, error) {
	if !x.env.has("osascript") {
		return "", missingHelper("reading the accessibility tree", "osascript")
	}
	out, err := x.env.Run(ctx, script, "osascript", "-l", "JavaScript", "-")
	if err != nil && strings.Contains(err.Error(), "assistive access") {
		return out, fmt.Errorf("%w — grant your terminal Accessibility permission in System Settings → Privacy & Security → Accessibility", err)
	}
	return out, err
}

// axPrelude finds the window and defines bulk, which reads one attribute of
// every element in a list in a single Apple Event, falling back to one event
// per element when the bulk read is refused (a list where some element lacks
// the attribute).
const axPrelude = `const se = Application('System Events');
function bulk(list, attr) {
  try { const v = list[attr](); if (Array.isArray(v)) return v; } catch (e) {}
  const out = [];
  for (let i = 0; i < list.length; i++) { try { out.push(list[i][attr]()); } catch (e) { out.push(null); } }
  return out;
}
function findWindow(target) {
  let proc = null;
  if (target && target.pid) { const m = se.processes.whose({unixId: target.pid}); if (m.length) proc = m[0]; }
  if (!proc) { const m = se.processes.whose({frontmost: true}); if (m.length) proc = m[0]; }
  if (!proc) throw new Error('no application is in front');
  const wins = proc.windows;
  let win = null;
  if (target && target.title) { for (let i = 0; i < wins.length; i++) { try { if (wins[i].name() === target.title) { win = wins[i]; break; } } catch (e) {} } }
  if (!win && wins.length) win = wins[0];
  if (!win) throw new Error(proc.name() + ' has no window');
  return [proc, win];
}
function describe(els, i, roles, subs, names, titles, descs, values, poss, sizes, ens, focs, sels, path) {
  const o = { r: roles[i] || '', sr: subs[i] || '', n: names[i] || titles[i] || descs[i] || '', path: path, en: ens[i] !== false, f: focs[i] === true, sel: sels[i] === true };
  if (o.r !== 'AXSecureTextField' && values[i] !== null && values[i] !== undefined && typeof values[i] !== 'object') { o.v = String(values[i]).slice(0, 256); }
  if (poss[i] && sizes[i]) { o.x = poss[i][0]; o.y = poss[i][1]; o.w = sizes[i][0]; o.h = sizes[i][1]; }
  return o;
}
`

// axReadScript walks one window level by level under a deadline.
func axReadScript(target *Window, budget time.Duration) string {
	t := "null"
	if target != nil {
		b, _ := json.Marshal(map[string]any{"pid": target.PID, "title": target.Title})
		t = string(b)
	}
	return axPrelude + fmt.Sprintf(`function run() {
  const pw = findWindow(%s); const proc = pw[0], win = pw[1];
  const deadline = Date.now() + %d;
  const out = []; let cut = false, n = 0;
  const queue = [[win, '']];
  while (queue.length) {
    if (Date.now() > deadline || n >= %d) { cut = true; break; }
    const [parent, path] = queue.shift();
    let kids; try { kids = parent.uiElements; if (!kids.length) continue; } catch (e) { continue; }
    const count = Math.min(kids.length, %d);
    const roles = bulk(kids, 'role'), subs = bulk(kids, 'subrole'), names = bulk(kids, 'name'), titles = bulk(kids, 'title'),
      descs = bulk(kids, 'accessibilityDescription'), values = bulk(kids, 'value'), poss = bulk(kids, 'position'), sizes = bulk(kids, 'size'),
      ens = bulk(kids, 'enabled'), focs = bulk(kids, 'focused'), sels = bulk(kids, 'selected');
    for (let i = 0; i < count; i++) {
      n++;
      const p = path === '' ? String(i) : path + '.' + i;
      const o = describe(kids, i, roles, subs, names, titles, descs, values, poss, sizes, ens, focs, sels, p);
      try { o.a = kids[i].actions.name(); } catch (e) { o.a = []; }
      out.push(o);
      queue.push([kids[i], p]);
    }
  }
  return JSON.stringify({ app: proc.name(), pid: proc.unixId(), title: win.name(), cut: cut, nodes: out });
}`, t, budget.Milliseconds(), treeMaxNodes, treeMaxChildren)
}

// axNode is one element as the script renders it.
type axNode struct {
	Role     string   `json:"r"`
	Subrole  string   `json:"sr"`
	Name     string   `json:"n"`
	Path     string   `json:"path"`
	Enabled  bool     `json:"en"`
	Focused  bool     `json:"f"`
	Selected bool     `json:"sel"`
	Value    *string  `json:"v"`
	X        *float64 `json:"x"`
	Y        *float64 `json:"y"`
	W        *float64 `json:"w"`
	H        *float64 `json:"h"`
	Actions  []string `json:"a"`
}

type axWindow struct {
	App   string   `json:"app"`
	PID   int      `json:"pid"`
	Title string   `json:"title"`
	Cut   bool     `json:"cut"`
	Nodes []axNode `json:"nodes"`
}

func (n axNode) can(action string) bool {
	for _, a := range n.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// axRole maps an AX role, refined by its subrole, onto the shared vocabulary.
func axRole(n axNode) string {
	switch n.Role {
	case "AXButton":
		if n.Subrole == "AXToggle" {
			return RoleToggle
		}
		return RoleButton
	case "AXDisclosureTriangle":
		return RoleButton
	case "AXCheckBox":
		if n.Subrole == "AXSwitch" {
			return RoleSwitch
		}
		return RoleCheckbox
	case "AXRadioButton":
		if n.Subrole == "AXTabButton" {
			return RoleTab
		}
		return RoleRadio
	case "AXMenuItem":
		return RoleMenuItem
	case "AXMenuBarItem":
		return RoleMenu
	case "AXPopUpButton", "AXComboBox", "AXMenuButton":
		return RoleCombobox
	case "AXTextField", "AXTextArea", "AXSearchField":
		if n.Subrole == "AXSecureTextField" {
			return RolePassword
		}
		return RoleTextField
	case "AXSecureTextField":
		return RolePassword
	case "AXIncrementor":
		return RoleSpin
	case "AXSlider":
		return RoleSlider
	case "AXLink":
		return RoleLink
	case "AXRow":
		if n.Subrole == "AXOutlineRow" {
			return RoleTreeItem
		}
		return RoleListItem
	case "AXCell":
		return RoleCell
	case "AXStaticText":
		return RoleLabel
	case "AXImage":
		return RoleIcon
	}
	if n.can("AXPress") {
		return RoleOther
	}
	return ""
}

// element renders a node in the shared shape.
func (n axNode) element(pid int, title string) Element {
	role := axRole(n)
	e := Element{
		Role: role, Name: strings.TrimSpace(n.Name),
		Focused: n.Focused, Disabled: !n.Enabled, Selected: n.Selected,
		Secret: role == RolePassword,
		Handle: fmt.Sprintf("%d|%s|%s", pid, n.Path, title),
	}
	if n.Value != nil && !e.Secret {
		switch role {
		case RoleCheckbox, RoleRadio, RoleSwitch, RoleToggle:
			e.Checked = triOf(*n.Value == "1")
		case RoleLabel:
			if e.Name == "" {
				e.Name = strings.TrimSpace(*n.Value)
			}
		default:
			e.Value = *n.Value
		}
	}
	if n.X != nil && n.Y != nil && n.W != nil && n.H != nil {
		e.Bounds = image.Rect(int(*n.X), int(*n.Y), int(*n.X+*n.W), int(*n.Y+*n.H))
	}
	if n.can("AXPress") || n.can("AXPick") || n.can("AXConfirm") {
		e.Actions = append(e.Actions, ActPress)
	}
	if role == RoleTextField || role == RolePassword || (role == RoleCombobox && n.Role == "AXComboBox") {
		e.Actions = append(e.Actions, ActSetText, ActFocus)
	}
	return e
}

func (x *axAccessor) Read(ctx context.Context, target *Window) (*Tree, error) {
	out, err := x.jxa(ctx, axReadScript(target, axBudget))
	if err != nil {
		return nil, fmt.Errorf("accessibility: %w", err)
	}
	var w axWindow
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &w); err != nil {
		return nil, fmt.Errorf("the System Events answer is not a tree: %v", err)
	}
	tree := &Tree{
		Backend: x.Name(), App: w.App, Window: w.Title, PID: w.PID,
		Positioned: true, Complete: !w.Cut, Nodes: len(w.Nodes),
	}
	if target != nil {
		tree.WindowID = target.ID
	}
	for _, n := range w.Nodes {
		if e := n.element(w.PID, w.Title); e.Role != "" {
			tree.Elements = append(tree.Elements, e)
		}
	}
	return tree, nil
}

// axHandle splits a handle into the process, the index path and the window.
func axHandle(h string) (pid int, path []int, title string, err error) {
	parts := strings.SplitN(h, "|", 3)
	if len(parts) != 3 {
		return 0, nil, "", fmt.Errorf("%w: not an AX element", ErrStale)
	}
	if pid, err = strconv.Atoi(parts[0]); err != nil || parts[1] == "" {
		return 0, nil, "", fmt.Errorf("%w: not an AX element", ErrStale)
	}
	for _, p := range strings.Split(parts[1], ".") {
		i, err := strconv.Atoi(p)
		if err != nil || i < 0 {
			return 0, nil, "", fmt.Errorf("%w: not an AX element", ErrStale)
		}
		path = append(path, i)
	}
	return pid, path, parts[2], nil
}

// axActScript finds an element by its path, checks it is still what was
// listed, acts, and describes it afterwards. The first line is the verdict.
func axActScript(el Element, act Act) (string, error) {
	pid, path, title, err := axHandle(el.Handle)
	if err != nil {
		return "", err
	}
	target, _ := json.Marshal(map[string]any{"pid": pid, "title": title})
	steps, _ := json.Marshal(path)
	text, _ := json.Marshal(act.Text)
	var body string
	switch act.Kind {
	case "":
		body = ``
	case ActPress:
		body = `const names = el.actions.name();
  const pick = ['AXPress', 'AXPick', 'AXConfirm'].find(a => names.indexOf(a) >= 0);
  if (!pick) return 'REFUSED it offers no press action';
  el.actions.byName(pick).perform();`
	case ActSetText:
		body = `try { el.value = ` + string(text) + `; } catch (e) { return 'REFUSED ' + e.message; }`
	case ActFocus:
		body = `try { el.focused = true; } catch (e) { return 'REFUSED ' + e.message; }`
	default:
		return "", fmt.Errorf("%w: unknown action %q", ErrRefused, act.Kind)
	}
	return axPrelude + fmt.Sprintf(`function run() {
  let el;
  try {
    const pw = findWindow(%s);
    el = pw[1];
    const path = %s;
    for (const i of path) { const kids = el.uiElements; if (i >= kids.length) return 'STALE'; el = kids[i]; }
  } catch (e) { return 'STALE'; }
  try {
  %s
  } catch (e) { return 'ERROR ' + e.message; }
  delay(0.06);
  const one = [el];
  try {
    const o = describe(one, 0, [el.role()], [bulk(one, 'subrole')[0]], [bulk(one, 'name')[0]], [bulk(one, 'title')[0]], [bulk(one, 'accessibilityDescription')[0]],
      [bulk(one, 'value')[0]], [bulk(one, 'position')[0]], [bulk(one, 'size')[0]], [bulk(one, 'enabled')[0]], [bulk(one, 'focused')[0]], [bulk(one, 'selected')[0]], %q);
    try { o.a = el.actions.name(); } catch (e) { o.a = []; }
    return 'OK\n' + JSON.stringify(o);
  } catch (e) { return 'OK\nGONE'; }
}`, target, steps, body, strings.Join(intsToStrings(path), ".")), nil
}

func intsToStrings(ns []int) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return out
}

func (x *axAccessor) run(ctx context.Context, el Element, act Act) (axNode, bool, error) {
	script, err := axActScript(el, act)
	if err != nil {
		return axNode{}, false, err
	}
	out, err := x.jxa(ctx, script)
	if err != nil {
		return axNode{}, false, fmt.Errorf("the action may or may not have reached the application: %w", err)
	}
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	first := strings.TrimSpace(lines[0])
	switch {
	case first == "STALE":
		return axNode{}, false, ErrStale
	case strings.HasPrefix(first, "REFUSED"):
		return axNode{}, false, fmt.Errorf("%w: %s", ErrRefused, strings.TrimSpace(strings.TrimPrefix(first, "REFUSED")))
	case strings.HasPrefix(first, "ERROR"):
		return axNode{}, false, fmt.Errorf("the action may or may not have reached the application: %s", strings.TrimSpace(strings.TrimPrefix(first, "ERROR")))
	case first != "OK":
		return axNode{}, false, fmt.Errorf("unexpected answer from System Events: %q", first)
	}
	if len(lines) < 2 || strings.TrimSpace(lines[1]) == "GONE" {
		return axNode{}, true, nil
	}
	var n axNode
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[1])), &n); err != nil {
		return axNode{}, true, nil
	}
	return n, false, nil
}

func (x *axAccessor) Refresh(ctx context.Context, el Element) (Element, error) {
	n, gone, err := x.run(ctx, el, Act{})
	if err != nil {
		return Element{}, err
	}
	if gone {
		return Element{}, ErrStale
	}
	pid, _, title, _ := axHandle(el.Handle)
	now := n.element(pid, title)
	if !sameElement(el, now) {
		return Element{}, fmt.Errorf("%w: its place now holds %s", ErrStale, now.Label())
	}
	now.Ref, now.Layer = el.Ref, el.Layer
	if now.Name == "" {
		now.Name = el.Name
	}
	return now, nil
}

func (x *axAccessor) Do(ctx context.Context, el Element, act Act) (Outcome, error) {
	if !el.Can(act.Kind) {
		return Outcome{}, fmt.Errorf("%w: %s offers no %s action", ErrRefused, el.Label(), act.Kind)
	}
	// The path is checked against what was listed before anything acts: an
	// index path is only an address, and a window that gained a row since
	// the read puts a different control at it.
	if _, err := x.Refresh(ctx, el); err != nil {
		return Outcome{}, err
	}
	n, gone, err := x.run(ctx, el, act)
	if err != nil {
		return Outcome{}, err
	}
	if gone {
		return Outcome{After: el, Gone: true}, nil
	}
	pid, _, title, _ := axHandle(el.Handle)
	after := n.element(pid, title)
	after.Ref, after.Layer = el.Ref, el.Layer
	if after.Name == "" {
		after.Name = el.Name
	}
	return Outcome{After: after}, nil
}
