package desktop

import (
	"context"
	"errors"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Windows and macOS backends are scripts run by the platform's own
// interpreter; these tests feed the Go side what those scripts print and
// check what it makes of it, and check the scripts it writes.

const uiaSample = `{"app":"notepad","title":"Untitled - Notepad","pid":77,"hwnd":4242,"cut":false,"nodes":[
{"ct":"ControlType.Window","n":"Untitled - Notepad","id":"","en":true,"f":false,"kf":true,"pw":false,"off":false,"rid":"42.1","ord":1,"x":0,"y":0,"w":800,"h":600,"p":[]},
{"ct":"ControlType.Edit","n":"Text Editor","id":"15","en":true,"f":true,"kf":true,"pw":false,"off":false,"rid":"42.2","ord":2,"x":10,"y":40,"w":780,"h":500,"p":["ValuePatternIdentifiers.Pattern","TextPatternIdentifiers.Pattern"],"ro":false,"v":"hello"},
{"ct":"ControlType.Edit","n":"PIN","id":"","en":true,"f":false,"kf":true,"pw":true,"off":false,"rid":"42.3","ord":3,"x":10,"y":550,"w":100,"h":20,"p":["ValuePatternIdentifiers.Pattern"],"ro":false},
{"ct":"ControlType.Button","n":"Bold","id":"b","en":true,"f":false,"kf":true,"pw":false,"off":false,"rid":"42.4","ord":4,"x":10,"y":5,"w":30,"h":30,"p":["TogglePatternIdentifiers.Pattern"],"t":"On"},
{"ct":"ControlType.ComboBox","n":"Font","id":"","en":true,"f":false,"kf":true,"pw":false,"off":false,"rid":"42.5","ord":5,"x":50,"y":5,"w":90,"h":30,"p":["ExpandCollapsePatternIdentifiers.Pattern","ValuePatternIdentifiers.Pattern"],"ec":"Collapsed","ro":true,"v":"Arial"},
{"ct":"ControlType.Button","n":"Close","id":"","en":false,"f":false,"kf":false,"pw":false,"off":false,"rid":"42.6","ord":6,"x":770,"y":5,"w":25,"h":25,"p":["InvokePatternIdentifiers.Pattern"]},
{"ct":"ControlType.Pane","n":"","id":"","en":true,"f":false,"kf":false,"pw":false,"off":false,"rid":"42.7","ord":7,"p":[]}
]}`

func TestUIAReadsATree(t *testing.T) {
	m := newMachine("windows", "powershell")
	m.outputs["powershell"] = uiaSample
	acc := NewAccessor(m.Env(), nil)
	tree, err := acc.Read(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tree.App != "notepad" || tree.WindowID != "4242" || tree.PID != 77 || !tree.Positioned || !tree.Complete {
		t.Fatalf("tree = %+v", tree)
	}
	script := m.lastCall()[len(m.lastCall())-1]
	for _, want := range []string{"[int64]'0'", "GetForegroundWindow", "ControlViewWalker", "ConvertTo-Json"} {
		if !strings.Contains(script, want) {
			t.Errorf("read script lacks %q", want)
		}
	}
	tree.finish(image.Rect(0, 0, 1920, 1080))
	got := labels(tree)
	want := `e1:toggle "Bold" e2:combobox "Font" e3:button "Close" e4:textfield "Text Editor" e5:password "PIN"`
	if got != want {
		t.Fatalf("elements:\n got %s\nwant %s", got, want)
	}
	bold, _ := tree.Find("e1")
	if bold.Checked != TriOn || !bold.Can(ActPress) {
		t.Errorf("bold = %+v", bold)
	}
	font, _ := tree.Find("e2")
	if font.Expanded != TriOff || !font.ReadOnly || font.Can(ActSetText) || !font.Can(ActPress) || !font.Can(ActExpand) {
		t.Errorf("font = %+v", font)
	}
	if closer, _ := tree.Find("e3"); !closer.Disabled {
		t.Errorf("close = %+v", closer)
	}
	if edit, _ := tree.Find("e4"); edit.Value != "hello" || !edit.Can(ActSetText) || edit.ID != "15" {
		t.Errorf("edit = %+v", edit)
	}
	if pin, _ := tree.Find("e5"); !pin.Secret || pin.Value != "" {
		t.Errorf("pin = %+v", pin)
	}

	if _, err := acc.Read(context.Background(), &Window{ID: "0x12"}); err == nil {
		t.Error("a window id that is not a handle was read")
	}
	if _, err := acc.Read(context.Background(), &Window{ID: "99"}); err != nil {
		t.Errorf("read by handle: %v", err)
	} else if !strings.Contains(m.lastCall()[len(m.lastCall())-1], "[int64]'99'") {
		t.Error("the handle was not passed")
	}
	m.outputs["powershell"] = "Exception calling FromHandle"
	if _, err := acc.Read(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not a tree") {
		t.Errorf("garbage: %v", err)
	}
	m.errs["powershell"] = errors.New("exit 1")
	if _, err := acc.Read(context.Background(), nil); err == nil {
		t.Error("a failed script read as a tree")
	}
	if _, err := newUIA(newMachine("windows").Env()).Read(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "powershell") {
		t.Errorf("no PowerShell: %v", err)
	}
}

func TestUIAActs(t *testing.T) {
	m := newMachine("windows", "powershell")
	acc := newUIA(m.Env())
	ctx := context.Background()
	edit := Element{Role: RoleTextField, Name: "Text Editor", ID: "15", Handle: "4242|42.2", Actions: []string{ActSetText, ActFocus}, Ref: "e4"}
	after := `{"ct":"ControlType.Edit","n":"Text Editor","id":"15","en":true,"f":true,"kf":true,"pw":false,"off":false,"rid":"42.2","ord":0,"x":10,"y":40,"w":780,"h":500,"p":["ValuePatternIdentifiers.Pattern"],"ro":false,"v":"new text"}`

	m.outputs["powershell"] = "OK\n" + after
	out, err := acc.Do(ctx, edit, Act{Kind: ActSetText, Text: "new text"})
	if err != nil || out.After.Value != "new text" || out.After.Ref != "e4" {
		t.Fatalf("set: %v %+v", err, out)
	}
	script := m.lastCall()[len(m.lastCall())-1]
	if !strings.Contains(script, "'42.2'") || !strings.Contains(script, "SetValue($in)") || strings.Contains(script, "new text") {
		t.Errorf("set script: %s", script)
	}
	if m.lastStdin() != "new text" {
		t.Errorf("the text did not ride stdin: %q", m.lastStdin())
	}

	for out, want := range map[string]error{
		"STALE":                   ErrStale,
		"REFUSED it is read-only": ErrRefused,
	} {
		m.outputs["powershell"] = out
		if _, err := acc.Do(ctx, edit, Act{Kind: ActSetText, Text: "x"}); !errors.Is(err, want) {
			t.Errorf("%s: %v", out, err)
		}
	}
	m.outputs["powershell"] = "ERROR access denied"
	if _, err := acc.Do(ctx, edit, Act{Kind: ActSetText, Text: "x"}); err == nil || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "may or may not") {
		t.Errorf("an error mid-action: %v", err)
	}
	m.outputs["powershell"] = "OK\nGONE"
	if out, err := acc.Do(ctx, edit, Act{Kind: ActSetText, Text: "x"}); err != nil || !out.Gone {
		t.Errorf("gone: %v %+v", err, out)
	}
	m.outputs["powershell"] = "huh"
	if _, err := acc.Do(ctx, edit, Act{Kind: ActSetText, Text: "x"}); err == nil {
		t.Error("an unknown verdict was accepted")
	}
	if _, err := acc.Do(ctx, edit, Act{Kind: ActPress}); !errors.Is(err, ErrRefused) {
		t.Errorf("an action the element does not offer: %v", err)
	}

	press := Element{Role: RoleButton, Name: "Bold", Handle: "4242|42.4", Actions: []string{ActPress, ActExpand, ActCollapse, ActFocus}}
	for _, kind := range []string{ActPress, ActExpand, ActCollapse, ActFocus} {
		m.outputs["powershell"] = "OK\nGONE"
		if _, err := acc.Do(ctx, press, Act{Kind: kind}); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}

	// Refresh checks identity on what the script reports.
	m.outputs["powershell"] = "OK\n" + after
	if now, err := acc.Refresh(ctx, edit); err != nil || now.Value != "new text" {
		t.Errorf("refresh: %v %+v", err, now)
	}
	m.outputs["powershell"] = "OK\n" + strings.Replace(after, `"Text Editor","id":"15"`, `"Other","id":"16"`, 1)
	if _, err := acc.Refresh(ctx, edit); !errors.Is(err, ErrStale) {
		t.Errorf("another element at the id: %v", err)
	}
	m.outputs["powershell"] = "OK\n" + strings.Replace(after, `"off":false`, `"off":true`, 1)
	if _, err := acc.Refresh(ctx, edit); !errors.Is(err, ErrStale) {
		t.Errorf("offscreen: %v", err)
	}
	m.outputs["powershell"] = "OK\nGONE"
	if _, err := acc.Refresh(ctx, edit); !errors.Is(err, ErrStale) {
		t.Errorf("gone: %v", err)
	}
	for _, bad := range []string{"", "x|1", "1|a.b", "1|"} {
		if _, err := acc.Refresh(ctx, Element{Handle: bad}); !errors.Is(err, ErrStale) {
			t.Errorf("handle %q: %v", bad, err)
		}
	}
	if _, err := uiaActScript("1|2", Act{Kind: "fly"}); !errors.Is(err, ErrRefused) {
		t.Errorf("unknown kind: %v", err)
	}
}

func TestUIARoles(t *testing.T) {
	for ct, want := range map[string]string{
		"CheckBox": RoleCheckbox, "RadioButton": RoleRadio, "MenuItem": RoleMenuItem, "Spinner": RoleSpin,
		"Slider": RoleSlider, "Hyperlink": RoleLink, "TabItem": RoleTab, "ListItem": RoleListItem,
		"DataItem": RoleListItem, "TreeItem": RoleTreeItem, "Text": RoleLabel, "Image": RoleIcon, "Document": RoleTextField,
		"SplitButton": RoleButton,
	} {
		if got := uiaRole(uiaNode{ControlType: "ControlType." + ct}); got != want {
			t.Errorf("%s = %s, want %s", ct, got, want)
		}
	}
	if uiaRole(uiaNode{ControlType: "ControlType.Custom", Patterns: []string{"InvokePatternIdentifiers.Pattern"}}) != RoleOther {
		t.Error("an invokable custom control")
	}
	n := uiaNode{ControlType: "ControlType.ListItem", Patterns: []string{"SelectionItemPatternIdentifiers.Pattern", "ExpandCollapsePatternIdentifiers.Pattern"}, Expand: "Expanded", Toggle: "Indeterminate", Selected: true}
	if e := n.element(1); !e.Can(ActPress) || e.Expanded != TriOn || e.Checked != TriOff || !e.Selected {
		t.Errorf("list item = %+v", e)
	}
}

const axSample = `{"app":"TextEdit","pid":501,"title":"Untitled","cut":true,"nodes":[
{"r":"AXTextArea","sr":"","n":"","path":"0","en":true,"f":true,"sel":false,"v":"draft","x":0,"y":30,"w":600,"h":400,"a":[]},
{"r":"AXButton","sr":"AXCloseButton","n":"close button","path":"1","en":true,"f":false,"sel":false,"x":5,"y":5,"w":14,"h":14,"a":["AXPress"]},
{"r":"AXCheckBox","sr":"","n":"Wrap","path":"2","en":true,"f":false,"sel":false,"v":"1","x":30,"y":5,"w":60,"h":14,"a":["AXPress"]},
{"r":"AXSecureTextField","sr":"","n":"Password","path":"3","en":true,"f":false,"sel":false,"x":100,"y":5,"w":100,"h":14,"a":[]},
{"r":"AXStaticText","sr":"","n":"","path":"4","en":true,"f":false,"sel":false,"v":"Status: ok","x":300,"y":5,"w":80,"h":14,"a":[]},
{"r":"AXGroup","sr":"","n":"","path":"5","en":true,"f":false,"sel":false,"a":[]}
]}`

func TestAXReadsATree(t *testing.T) {
	m := newMachine("darwin", "osascript")
	m.outputs["osascript"] = axSample
	acc := NewAccessor(m.Env(), nil)
	tree, err := acc.Read(context.Background(), &Window{ID: "w1", PID: 501, Title: "Untitled"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "osascript -l JavaScript -" {
		t.Errorf("ran %q", got)
	}
	if !strings.Contains(m.lastStdin(), `{"pid":501,"title":"Untitled"}`) {
		t.Errorf("the target did not reach the script")
	}
	if tree.Complete || tree.WindowID != "w1" || tree.App != "TextEdit" {
		t.Fatalf("tree = %+v", tree)
	}
	tree.finish(image.Rect(0, 0, 1440, 900))
	got := labels(tree)
	want := `e1:button "close button" e2:checkbox "Wrap" e3:password "Password" e4:textfield (unnamed)`
	if got != want {
		t.Fatalf("elements:\n got %s\nwant %s", got, want)
	}
	if wrap, _ := tree.Find("e2"); wrap.Checked != TriOn {
		t.Errorf("wrap = %+v", wrap)
	}
	if area, _ := tree.Find("e4"); area.Value != "draft" || !area.Can(ActSetText) || !area.Focused {
		t.Errorf("text area = %+v", area)
	}
	m.outputs["osascript"] = "not json"
	if _, err := acc.Read(context.Background(), nil); err == nil {
		t.Error("garbage read as a tree")
	}
	m.errs["osascript"] = errors.New("osascript: not allowed assistive access")
	if _, err := acc.Read(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "Privacy & Security") {
		t.Errorf("permission: %v", err)
	}
	if _, err := newAX(newMachine("darwin").Env()).Read(context.Background(), nil); err == nil {
		t.Error("read with no osascript")
	}
}

func TestAXActs(t *testing.T) {
	m := newMachine("darwin", "osascript")
	acc := newAX(m.Env())
	ctx := context.Background()
	wrap := Element{Role: RoleCheckbox, Name: "Wrap", Handle: "501|0.2|Untitled", Actions: []string{ActPress}, Ref: "e2"}
	listed := `OK
{"r":"AXCheckBox","sr":"","n":"Wrap","path":"0.2","en":true,"f":false,"sel":false,"v":"1","x":30,"y":5,"w":60,"h":14,"a":["AXPress"]}`
	m.outputs["osascript"] = listed
	out, err := acc.Do(ctx, wrap, Act{Kind: ActPress})
	if err != nil || out.After.Checked != TriOn || out.After.Ref != "e2" {
		t.Fatalf("press: %v %+v", err, out)
	}
	if !strings.Contains(m.lastStdin(), "AXPress") || !strings.Contains(m.lastStdin(), "[0,2]") {
		t.Errorf("press script: %s", m.lastStdin())
	}
	m.outputs["osascript"] = "OK\nGONE"
	if _, err := acc.Do(ctx, wrap, Act{Kind: ActPress}); !errors.Is(err, ErrStale) {
		t.Errorf("gone before acting: %v", err)
	}
	for out, want := range map[string]error{"STALE": ErrStale, "REFUSED nope": ErrRefused} {
		m.outputs["osascript"] = out
		if _, err := acc.Refresh(ctx, wrap); !errors.Is(err, want) {
			t.Errorf("%s: %v", out, err)
		}
	}
	m.outputs["osascript"] = "ERROR busy"
	if _, err := acc.Refresh(ctx, wrap); err == nil || !strings.Contains(err.Error(), "may or may not") {
		t.Errorf("error: %v", err)
	}
	m.outputs["osascript"] = "what"
	if _, err := acc.Refresh(ctx, wrap); err == nil {
		t.Error("an unknown verdict")
	}
	m.outputs["osascript"] = strings.Replace(listed, `"n":"Wrap"`, `"n":"Other"`, 1)
	if _, err := acc.Refresh(ctx, wrap); !errors.Is(err, ErrStale) {
		t.Errorf("another control at the path: %v", err)
	}
	if _, err := acc.Do(ctx, wrap, Act{Kind: ActSetText}); !errors.Is(err, ErrRefused) {
		t.Errorf("an action not offered: %v", err)
	}
	field := Element{Role: RoleTextField, Name: "", Handle: "501|0|Untitled", Actions: []string{ActSetText, ActFocus}}
	m.outputs["osascript"] = "OK\n{\"r\":\"AXTextArea\",\"path\":\"0\",\"en\":true,\"v\":\"it's \\\"quoted\\\"\",\"a\":[]}"
	out, err = acc.Do(ctx, field, Act{Kind: ActSetText, Text: `it's "quoted"`})
	if err != nil || out.After.Value != `it's "quoted"` {
		t.Fatalf("set: %v %+v", err, out)
	}
	if !strings.Contains(m.lastStdin(), `el.value = "it's \"quoted\""`) {
		t.Errorf("the text is not a safe JavaScript literal: %s", m.lastStdin())
	}
	if _, err := acc.Do(ctx, field, Act{Kind: ActFocus}); err != nil {
		t.Errorf("focus: %v", err)
	}
	for _, bad := range []string{"", "x|0|w", "1||w", "1|a|w"} {
		if _, err := acc.Refresh(ctx, Element{Handle: bad}); !errors.Is(err, ErrStale) {
			t.Errorf("handle %q: %v", bad, err)
		}
	}
	if _, err := axActScript(field, Act{Kind: "fly"}); !errors.Is(err, ErrRefused) {
		t.Errorf("unknown kind: %v", err)
	}
	for r, want := range map[string]string{
		"AXDisclosureTriangle": RoleButton, "AXRadioButton": RoleRadio, "AXMenuItem": RoleMenuItem, "AXMenuBarItem": RoleMenu,
		"AXPopUpButton": RoleCombobox, "AXIncrementor": RoleSpin, "AXSlider": RoleSlider, "AXLink": RoleLink,
		"AXRow": RoleListItem, "AXCell": RoleCell, "AXImage": RoleIcon,
	} {
		if got := axRole(axNode{Role: r}); got != want {
			t.Errorf("%s = %s", r, got)
		}
	}
	for _, c := range []struct {
		n    axNode
		want string
	}{
		{axNode{Role: "AXButton", Subrole: "AXToggle"}, RoleToggle},
		{axNode{Role: "AXCheckBox", Subrole: "AXSwitch"}, RoleSwitch},
		{axNode{Role: "AXRadioButton", Subrole: "AXTabButton"}, RoleTab},
		{axNode{Role: "AXRow", Subrole: "AXOutlineRow"}, RoleTreeItem},
		{axNode{Role: "AXTextField", Subrole: "AXSecureTextField"}, RolePassword},
	} {
		if got := axRole(c.n); got != c.want {
			t.Errorf("%+v = %s", c.n, got)
		}
	}
}

// The scripts are JavaScript, and a syntax error in one is a backend that
// never answers on any Mac. Node parses the same language.
func TestAXScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	el := Element{Handle: "501|0.3|Untitled"}
	scripts := map[string]string{"read": axReadScript(&Window{PID: 1, Title: "x"}, axBudget), "read-front": axReadScript(nil, axBudget)}
	for _, kind := range []string{"", ActPress, ActSetText, ActFocus} {
		s, err := axActScript(el, Act{Kind: kind, Text: "a\"b\n"})
		if err != nil {
			t.Fatal(err)
		}
		scripts["act-"+kind] = s
	}
	dir := t.TempDir()
	for name, s := range scripts {
		path := filepath.Join(dir, name+".js")
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", name, err, out)
		}
	}
}
