package desktop

// The Windows accessibility backend: UI Automation, driven from the
// PowerShell every Windows box already has. System.Windows.Automation ships
// with the .NET Framework PowerShell runs on, so nothing is installed and
// nothing is linked — the same bargain the rest of the Windows controller
// makes.
//
// An element is found again by its RuntimeId, which UI Automation keeps
// unique for the element's lifetime, searched for under the window it was
// read in; an element whose id no longer exists there is stale.

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"strconv"
	"strings"
	"time"
)

// uiaBudget is how long one tree read may take inside the script. PowerShell
// and the automation assemblies take most of a second to start, and that is
// paid before the walk begins.
const uiaBudget = 2500 * time.Millisecond

type uiaAccessor struct{ env Env }

func newUIA(env Env) Accessor { return &uiaAccessor{env: env} }

func (u *uiaAccessor) Name() string { return "uia" }

// Budget is the read deadline screen_view gives this backend.
func (u *uiaAccessor) Budget() time.Duration { return uiaBudget + 1500*time.Millisecond }

func (u *uiaAccessor) ps(ctx context.Context, script, stdin string) (string, error) {
	shell, ok := u.env.first("powershell", "pwsh")
	if !ok {
		return "", missingHelper("reading the accessibility tree", "powershell")
	}
	return u.env.Run(ctx, stdin, shell, "-NoProfile", "-NonInteractive", "-Command", script)
}

// uiaPrelude loads UI Automation and defines Describe, which renders one
// element as the record Go reads — the same shape for a read and for the
// element an action leaves behind. Coordinates are made relative to the
// virtual screen's top-left, the origin every other Factor coordinate uses.
//
// The client-side providers are registered by hand: they are what turn a
// Win32 or Windows Forms control into a button or a field, and PowerShell's
// managed client never loads them itself, so every such control read as a
// bare pane and Notepad as a window with nothing in it. Applications with a
// provider of their own (XAML, WPF, browsers) never needed them, which is
// why the gap only showed on classic windows. The registration only takes
// once the client is up — before that it throws a NullReferenceException —
// so RootElement is touched first.
const uiaPrelude = psPrelude + `Add-Type -AssemblyName UIAutomationClient
Add-Type -AssemblyName UIAutomationTypes
$A = [System.Windows.Automation.AutomationElement]
try { $null = $A::RootElement; Add-Type -AssemblyName UIAutomationClientsideProviders; [System.Windows.Automation.ClientSettings]::RegisterClientSideProviders([UIAutomationClientsideProviders.UIAutomationClientSideProviders]::ClientSideProviderDescriptionTable) } catch { }
function Describe($el, $ord) {
  $c = $el.Current
  $o = [ordered]@{ ct = $c.ControlType.ProgrammaticName; n = $c.Name; id = $c.AutomationId; en = $c.IsEnabled; f = $c.HasKeyboardFocus; kf = $c.IsKeyboardFocusable; pw = $c.IsPassword; off = $c.IsOffscreen; rid = (($el.GetRuntimeId()) -join '.'); ord = $ord }
  $r = $c.BoundingRectangle
  if (-not $r.IsEmpty) { $o.x = [int]$r.X - $vs.Left; $o.y = [int]$r.Y - $vs.Top; $o.w = [int]$r.Width; $o.h = [int]$r.Height }
  $p = @($el.GetSupportedPatterns() | ForEach-Object { $_.ProgrammaticName })
  $o.p = $p
  if ($p -contains 'TogglePatternIdentifiers.Pattern') { $o.t = [string]$el.GetCurrentPattern([System.Windows.Automation.TogglePattern]::Pattern).Current.ToggleState }
  if ($p -contains 'ValuePatternIdentifiers.Pattern') { $vp = $el.GetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern).Current; $o.ro = $vp.IsReadOnly; if (-not $c.IsPassword) { $v = [string]$vp.Value; if ($v.Length -gt 256) { $v = $v.Substring(0, 256) }; $o.v = $v } }
  if ($p -contains 'ExpandCollapsePatternIdentifiers.Pattern') { $o.ec = [string]$el.GetCurrentPattern([System.Windows.Automation.ExpandCollapsePattern]::Pattern).Current.ExpandCollapseState }
  if ($p -contains 'SelectionItemPatternIdentifiers.Pattern') { $o.s = $el.GetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern).Current.IsSelected }
  return $o
}
`

// uiaReadScript walks one window's control view, depth first and bounded in
// both time and nodes, skipping what is off screen.
func uiaReadScript(hwnd string, budget time.Duration) string {
	return uiaPrelude + fmt.Sprintf(`$h = [IntPtr]::new([int64]'%s')
if ($h -eq [IntPtr]::Zero) { $h = [FactorWin]::GetForegroundWindow() }
$root = $A::FromHandle($h)
$deadline = (Get-Date).AddMilliseconds(%d)
$walker = [System.Windows.Automation.TreeWalker]::ControlViewWalker
$out = New-Object System.Collections.Generic.List[object]
$stack = New-Object System.Collections.Stack
$stack.Push(@($root, 0))
$n = 0; $cut = $false
while ($stack.Count -gt 0) {
  if ((Get-Date) -gt $deadline -or $n -ge %d) { $cut = $true; break }
  $pair = $stack.Pop(); $el = $pair[0]; $d = $pair[1]; $n++
  try {
    if ($d -gt 0 -and $el.Current.IsOffscreen) { continue }
    $out.Add((Describe $el $n))
    $kids = New-Object System.Collections.Generic.List[object]
    $k = $walker.GetFirstChild($el)
    while ($k -ne $null -and $kids.Count -lt %d) { $kids.Add($k); $k = $walker.GetNextSibling($k) }
    for ($i = $kids.Count - 1; $i -ge 0; $i--) { $stack.Push(@($kids[$i], ($d + 1))) }
  } catch { }
}
$rc = $root.Current
$app = ''
try { $app = (Get-Process -Id $rc.ProcessId).ProcessName } catch { }
@{ app = $app; title = $rc.Name; pid = $rc.ProcessId; hwnd = [int64]$h; cut = $cut; nodes = $out } | ConvertTo-Json -Compress -Depth 4`,
		hwnd, budget.Milliseconds(), treeMaxNodes, treeMaxChildren)
}

// uiaNode is one element as the script renders it.
type uiaNode struct {
	ControlType string   `json:"ct"`
	Name        string   `json:"n"`
	ID          string   `json:"id"`
	Enabled     bool     `json:"en"`
	Focused     bool     `json:"f"`
	Focusable   bool     `json:"kf"`
	Password    bool     `json:"pw"`
	Offscreen   bool     `json:"off"`
	RuntimeID   string   `json:"rid"`
	Order       int      `json:"ord"`
	X           *int     `json:"x"`
	Y           *int     `json:"y"`
	W           *int     `json:"w"`
	H           *int     `json:"h"`
	Patterns    []string `json:"p"`
	Toggle      string   `json:"t"`
	ReadOnly    bool     `json:"ro"`
	Value       string   `json:"v"`
	Expand      string   `json:"ec"`
	Selected    bool     `json:"s"`
}

type uiaWindow struct {
	App   string    `json:"app"`
	Title string    `json:"title"`
	PID   int       `json:"pid"`
	HWND  int64     `json:"hwnd"`
	Cut   bool      `json:"cut"`
	Nodes []uiaNode `json:"nodes"`
}

func (n uiaNode) has(pattern string) bool {
	for _, p := range n.Patterns {
		if p == pattern+"PatternIdentifiers.Pattern" {
			return true
		}
	}
	return false
}

// uiaRole maps a control type onto the shared vocabulary, refined by the
// patterns it supports: UI Automation has no toggle-button type, only a
// button that can toggle.
func uiaRole(n uiaNode) string {
	switch strings.TrimPrefix(n.ControlType, "ControlType.") {
	case "Button", "SplitButton":
		if n.has("Toggle") {
			return RoleToggle
		}
		return RoleButton
	case "CheckBox":
		return RoleCheckbox
	case "RadioButton":
		return RoleRadio
	case "MenuItem":
		return RoleMenuItem
	case "ComboBox":
		return RoleCombobox
	case "Edit", "Document":
		if n.Password {
			return RolePassword
		}
		return RoleTextField
	case "Spinner":
		return RoleSpin
	case "Slider":
		return RoleSlider
	case "Hyperlink":
		return RoleLink
	case "TabItem":
		return RoleTab
	case "ListItem", "DataItem":
		return RoleListItem
	case "TreeItem":
		return RoleTreeItem
	case "Text":
		return RoleLabel
	case "Image":
		return RoleIcon
	}
	if n.has("Invoke") {
		return RoleOther
	}
	return ""
}

// element renders a node in the shared shape.
func (n uiaNode) element(hwnd int64) Element {
	role := uiaRole(n)
	e := Element{
		Role: role, Name: strings.TrimSpace(n.Name), ID: n.ID,
		Focused: n.Focused, Disabled: !n.Enabled, Selected: n.Selected,
		ReadOnly: n.ReadOnly, Secret: n.Password || role == RolePassword,
		Handle: fmt.Sprintf("%d|%s", hwnd, n.RuntimeID),
	}
	if !e.Secret {
		e.Value = n.Value
	}
	if n.X != nil && n.Y != nil && n.W != nil && n.H != nil {
		e.Bounds = image.Rect(*n.X, *n.Y, *n.X+*n.W, *n.Y+*n.H)
	}
	switch n.Toggle {
	case "On":
		e.Checked = TriOn
	case "Off", "Indeterminate":
		e.Checked = TriOff
	}
	switch n.Expand {
	case "Expanded", "PartiallyExpanded":
		e.Expanded = TriOn
	case "Collapsed":
		e.Expanded = TriOff
	}
	if n.has("Invoke") || n.has("Toggle") || (n.has("SelectionItem") && role != RoleTextField) {
		e.Actions = append(e.Actions, ActPress)
	}
	if n.has("Value") && !n.ReadOnly {
		e.Actions = append(e.Actions, ActSetText)
	}
	if n.Focusable {
		e.Actions = append(e.Actions, ActFocus)
	}
	if n.has("ExpandCollapse") {
		e.Actions = append(e.Actions, ActExpand, ActCollapse)
		if !e.Can(ActPress) && role == RoleCombobox {
			// A combo box opens by expanding: that is what clicking it does.
			e.Actions = append(e.Actions, ActPress)
		}
	}
	return e
}

func (u *uiaAccessor) Read(ctx context.Context, target *Window) (*Tree, error) {
	hwnd := "0"
	if target != nil {
		hwnd = target.ID
	}
	if _, err := strconv.ParseInt(hwnd, 10, 64); err != nil {
		return nil, fmt.Errorf("window %q has no handle UI Automation can open", hwnd)
	}
	out, err := u.ps(ctx, uiaReadScript(hwnd, uiaBudget), "")
	if err != nil {
		return nil, fmt.Errorf("UI Automation: %w", err)
	}
	var w uiaWindow
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &w); err != nil {
		return nil, fmt.Errorf("UI Automation answered something that is not a tree: %v", err)
	}
	tree := &Tree{
		Backend: u.Name(), App: w.App, Window: w.Title, PID: w.PID,
		WindowID: strconv.FormatInt(w.HWND, 10), Positioned: true,
		Complete: !w.Cut, Nodes: len(w.Nodes),
	}
	for i, n := range w.Nodes {
		if i == 0 {
			continue // the window itself
		}
		if e := n.element(w.HWND); e.Role != "" {
			tree.Elements = append(tree.Elements, e)
		}
	}
	return tree, nil
}

// uiaFind locates an element again from its handle, or prints STALE.
func uiaFind(handle string) (string, error) {
	hwnd, rid, ok := strings.Cut(handle, "|")
	if !ok || hwnd == "" || rid == "" {
		return "", fmt.Errorf("%w: not a UI Automation element", ErrStale)
	}
	if _, err := strconv.ParseInt(hwnd, 10, 64); err != nil {
		return "", fmt.Errorf("%w: not a UI Automation element", ErrStale)
	}
	for _, part := range strings.Split(rid, ".") {
		if _, err := strconv.Atoi(part); err != nil {
			return "", fmt.Errorf("%w: not a UI Automation element", ErrStale)
		}
	}
	return fmt.Sprintf(`$root = $A::FromHandle([IntPtr]::new([int64]'%s'))
$rid = [int[]]('%s'.Split('.'))
$cond = New-Object System.Windows.Automation.PropertyCondition($A::RuntimeIdProperty, $rid)
$el = $root.FindFirst([System.Windows.Automation.TreeScope]::Subtree, $cond)
if ($el -eq $null) { 'STALE'; exit }
`, hwnd, rid), nil
}

// uiaActScript performs one action on a found element and describes it
// afterwards. The first line is the verdict Go branches on.
func uiaActScript(handle string, act Act) (string, error) {
	find, err := uiaFind(handle)
	if err != nil {
		return "", err
	}
	var body string
	switch act.Kind {
	case "":
		body = `'OK'`
	case ActPress:
		// Each elseif opens on the line its predecessor closes: a newline
		// before one ends the if statement in an interactive parse.
		body = `$p = @($el.GetSupportedPatterns() | ForEach-Object { $_.ProgrammaticName })
if ($p -contains 'InvokePatternIdentifiers.Pattern') { $el.GetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern).Invoke()
} elseif ($p -contains 'TogglePatternIdentifiers.Pattern') { $el.GetCurrentPattern([System.Windows.Automation.TogglePattern]::Pattern).Toggle()
} elseif ($p -contains 'SelectionItemPatternIdentifiers.Pattern') { $el.GetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern).Select()
} elseif ($p -contains 'ExpandCollapsePatternIdentifiers.Pattern') { $ec = $el.GetCurrentPattern([System.Windows.Automation.ExpandCollapsePattern]::Pattern); if ([string]$ec.Current.ExpandCollapseState -eq 'Collapsed') { $ec.Expand() } else { $ec.Collapse() }
} else { 'REFUSED it offers no action'; exit }
'OK'`
	case ActSetText:
		body = psStdin + `$vp = $el.GetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern)
if ($vp.Current.IsReadOnly) { 'REFUSED it is read-only'; exit }
$vp.SetValue($in)
'OK'`
	case ActFocus:
		body = `$el.SetFocus()
'OK'`
	case ActExpand, ActCollapse:
		method := "Expand"
		if act.Kind == ActCollapse {
			method = "Collapse"
		}
		body = `$ec = $el.GetCurrentPattern([System.Windows.Automation.ExpandCollapsePattern]::Pattern)
$ec.` + method + `()
'OK'`
	default:
		return "", fmt.Errorf("%w: unknown action %q", ErrRefused, act.Kind)
	}
	return uiaPrelude + find + `try {
` + body + `
} catch [System.Windows.Automation.ElementNotAvailableException] { 'STALE'; exit
} catch [System.InvalidOperationException] { 'REFUSED ' + $_.Exception.Message; exit
} catch { 'ERROR ' + $_.Exception.Message; exit }
Start-Sleep -Milliseconds 60
try { (Describe $el 0) | ConvertTo-Json -Compress -Depth 3 } catch { 'GONE' }`, nil
}

// uiaVerdict reads an action script's answer.
func (u *uiaAccessor) run(ctx context.Context, el Element, act Act) (uiaNode, bool, error) {
	script, err := uiaActScript(el.Handle, act)
	if err != nil {
		return uiaNode{}, false, err
	}
	out, err := u.ps(ctx, script, act.Text)
	if err != nil {
		return uiaNode{}, false, fmt.Errorf("the action may or may not have reached the application: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	first := strings.TrimSpace(lines[0])
	switch {
	case first == "STALE":
		return uiaNode{}, false, ErrStale
	case strings.HasPrefix(first, "REFUSED"):
		return uiaNode{}, false, fmt.Errorf("%w: %s", ErrRefused, strings.TrimSpace(strings.TrimPrefix(first, "REFUSED")))
	case strings.HasPrefix(first, "ERROR"):
		return uiaNode{}, false, fmt.Errorf("the action may or may not have reached the application: %s", strings.TrimSpace(strings.TrimPrefix(first, "ERROR")))
	case first != "OK":
		return uiaNode{}, false, fmt.Errorf("UI Automation answered %q", first)
	}
	if len(lines) < 2 || strings.TrimSpace(lines[1]) == "GONE" {
		return uiaNode{}, true, nil
	}
	var n uiaNode
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[1])), &n); err != nil {
		return uiaNode{}, true, nil
	}
	return n, false, nil
}

func (u *uiaAccessor) Refresh(ctx context.Context, el Element) (Element, error) {
	n, gone, err := u.run(ctx, el, Act{})
	if err != nil {
		return Element{}, err
	}
	if gone {
		return Element{}, ErrStale
	}
	hwnd, _, _ := strings.Cut(el.Handle, "|")
	h, _ := strconv.ParseInt(hwnd, 10, 64)
	now := n.element(h)
	if n.Offscreen {
		return Element{}, fmt.Errorf("%w: it is no longer on screen", ErrStale)
	}
	if !sameElement(el, now) {
		return Element{}, fmt.Errorf("%w: its id now holds %s", ErrStale, now.Label())
	}
	now.Ref, now.Layer = el.Ref, el.Layer
	if now.Name == "" {
		now.Name = el.Name
	}
	return now, nil
}

func (u *uiaAccessor) Do(ctx context.Context, el Element, act Act) (Outcome, error) {
	if !el.Can(act.Kind) {
		return Outcome{}, fmt.Errorf("%w: %s offers no %s action", ErrRefused, el.Label(), act.Kind)
	}
	n, gone, err := u.run(ctx, el, act)
	if err != nil {
		return Outcome{}, err
	}
	if gone {
		return Outcome{After: el, Gone: true}, nil
	}
	hwnd, _, _ := strings.Cut(el.Handle, "|")
	h, _ := strconv.ParseInt(hwnd, 10, 64)
	after := n.element(h)
	after.Ref, after.Layer = el.Ref, el.Layer
	if after.Name == "" {
		after.Name = el.Name
	}
	return Outcome{After: after}, nil
}
