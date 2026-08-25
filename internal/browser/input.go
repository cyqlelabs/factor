//go:build !nobrowser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"

	"github.com/cyqlelabs/factor/internal/tools"
)

// findFileInput locates the file input a visible control stands in front of.
// Sites do not put <input type=file> on the page for a person to click: they
// hide it behind a styled button and forward the click, so the element the
// model can see is never the element the file has to be handed to. Search
// order is nearest-first from the named control, then the whole page.
const findFileInput = `(() => {
  const TARGET = %s;
  const cssPath = (el) => {
    const parts = [];
    while (el && el.nodeType === 1 && parts.length < 8) {
      let part = el.tagName.toLowerCase();
      if (el.id) { parts.unshift(part + '#' + CSS.escape(el.id)); break; }
      const parent = el.parentNode;
      if (parent) {
        const idx = Array.prototype.indexOf.call(parent.children, el) + 1;
        part += ':nth-child(' + idx + ')';
      }
      parts.unshift(part);
      el = el.parentNode;
    }
    return parts.join(' > ');
  };
  const inputs = Array.from(document.querySelectorAll('input[type=file]'));
  if (inputs.length === 0) return {error: 'no file input on this page'};
  if (TARGET) {
    const anchor = document.querySelector(TARGET);
    if (!anchor) return {error: 'no element matches ' + TARGET};
    // Walk outwards from the control: the input is usually a sibling or a
    // cousin a couple of levels up, inside the same widget.
    for (let node = anchor; node; node = node.parentElement) {
      const found = node.querySelector ? node.querySelector('input[type=file]') : null;
      if (found) return {selector: cssPath(found), count: inputs.length};
    }
  }
  return {selector: cssPath(inputs[0]), count: inputs.length};
})()`

type uploadTool struct{ s *Session }

func (t *uploadTool) Name() string { return "browser_upload" }
func (t *uploadTool) Description() string {
	return "Attach a file from disk to the page — an email attachment, an upload form, a profile picture. Point target at the visible control (the 'Attach' button or the drop area, as a ref from browser_read or a CSS selector) and this finds the file input behind it, which is normally hidden and never clickable. Use this rather than clicking through the operating system's file chooser."
}
func (t *uploadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "Path to the file to attach"},
			"target": map[string]any{"type": "string", "description": "Ref or CSS selector of the visible upload control; omit to use the page's only file input"},
		},
		"required": []any{"path"},
	}
}

func (t *uploadTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	path := tools.StringArg(args, "path")
	if t.s.guard != nil {
		resolved, err := t.s.guard.CheckRead(path)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		path = resolved
	}
	info, err := os.Stat(path)
	if err != nil {
		return tools.Errorf("cannot attach %s: %v", path, err)
	}
	if info.IsDir() {
		return tools.Errorf("cannot attach %s: it is a directory", path)
	}
	if info.Size() == 0 {
		return tools.Errorf("cannot attach %s: the file is empty", path)
	}

	var target string
	if raw := tools.StringArg(args, "target"); raw != "" {
		if why := t.s.staleRef(ctx, raw); why != "" {
			return tools.Errorf("upload refused: %s", why)
		}
		target = t.s.selectorFor(raw)
	}
	targetJSON, _ := json.Marshal(target)
	var found struct {
		Selector string `json:"selector"`
		Count    int    `json:"count"`
		Error    string `json:"error"`
	}
	if err := t.s.run(ctx, 20*time.Second,
		chromedp.Evaluate(fmt.Sprintf(findFileInput, targetJSON), &found)); err != nil {
		return tools.Errorf("looking for the upload field failed: %v", err)
	}
	if found.Error != "" {
		return tools.Errorf("cannot attach the file: %s", found.Error)
	}

	// The file is handed to the input over the protocol rather than typed into
	// a file chooser: the chooser is an operating-system window, not part of
	// the page, and nothing in the browser suite can reach it.
	if err := t.s.run(ctx, 30*time.Second, chromedp.ActionFunc(func(c context.Context) error {
		root, err := dom.GetDocument().Do(c)
		if err != nil {
			return err
		}
		nodeID, err := dom.QuerySelector(root.NodeID, found.Selector).Do(c)
		if err != nil {
			return err
		}
		if nodeID == 0 {
			return fmt.Errorf("the upload field went away before the file reached it")
		}
		return dom.SetFileInputFiles([]string{path}).WithNodeID(nodeID).Do(c)
	})); err != nil {
		return tools.Errorf("attaching %s failed: %v", path, err)
	}
	// Uploads are asynchronous on most sites: the page reads the file, shows a
	// progress row, and only then counts it as attached. Verify before saying
	// it worked, because "attached" is exactly the claim a user will not check.
	time.Sleep(1500 * time.Millisecond)
	r, err := t.s.read(ctx)
	if err != nil {
		return tools.Textf("Attached %s. The page could not be re-read afterwards (%v), so check it before sending.", path, err)
	}
	return tools.Text(fmt.Sprintf("Attached %s to the page. Confirm it appears below before sending.\n\n%s", path, formatRead(r)))
}

// namedKeys are the keys worth naming: the ones a page listens for that no
// character can stand in for.
var namedKeys = map[string]struct {
	key  string
	code string
	vk   int64
}{
	"enter":      {"Enter", "Enter", 13},
	"tab":        {"Tab", "Tab", 9},
	"escape":     {"Escape", "Escape", 27},
	"esc":        {"Escape", "Escape", 27},
	"backspace":  {"Backspace", "Backspace", 8},
	"delete":     {"Delete", "Delete", 46},
	"space":      {"Space", "Space", 32},
	"arrowup":    {"ArrowUp", "ArrowUp", 38},
	"arrowdown":  {"ArrowDown", "ArrowDown", 40},
	"arrowleft":  {"ArrowLeft", "ArrowLeft", 37},
	"arrowright": {"ArrowRight", "ArrowRight", 39},
	"up":         {"ArrowUp", "ArrowUp", 38},
	"down":       {"ArrowDown", "ArrowDown", 40},
	"left":       {"ArrowLeft", "ArrowLeft", 37},
	"right":      {"ArrowRight", "ArrowRight", 39},
	"home":       {"Home", "Home", 36},
	"end":        {"End", "End", 35},
	"pageup":     {"PageUp", "PageUp", 33},
	"pagedown":   {"PageDown", "PageDown", 34},
}

var modifierNames = map[string]input.Modifier{
	"alt": input.ModifierAlt, "option": input.ModifierAlt,
	"control": input.ModifierCtrl, "ctrl": input.ModifierCtrl,
	"meta": input.ModifierCommand, "cmd": input.ModifierCommand, "command": input.ModifierCommand, "super": input.ModifierCommand,
	"shift": input.ModifierShift,
}

// parseChord turns "Control+Enter" into what the protocol wants. A chord is
// one key with modifiers held, which is what a keyboard shortcut is; typing
// words is browser_fill's job.
func parseChord(chord string) (input.Modifier, string, string, int64, error) {
	parts := strings.Split(chord, "+")
	var mods input.Modifier
	for _, p := range parts[:len(parts)-1] {
		m, ok := modifierNames[strings.ToLower(strings.TrimSpace(p))]
		if !ok {
			return 0, "", "", 0, fmt.Errorf("unknown modifier %q", strings.TrimSpace(p))
		}
		mods |= m
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	if last == "" {
		return 0, "", "", 0, fmt.Errorf("no key named in %q", chord)
	}
	if k, ok := namedKeys[strings.ToLower(last)]; ok {
		return mods, k.key, k.code, k.vk, nil
	}
	if len([]rune(last)) != 1 {
		return 0, "", "", 0, fmt.Errorf("%q is not a single key — name one key, with optional modifiers, like Control+Enter", last)
	}
	r := []rune(last)[0]
	upper := []rune(strings.ToUpper(last))[0]
	code := "Key" + string(upper)
	if r >= '0' && r <= '9' {
		code = "Digit" + string(r)
	}
	return mods, string(r), code, int64(upper), nil
}

type keysTool struct{ s *Session }

func (t *keysTool) Name() string { return "browser_keys" }
func (t *keysTool) Description() string {
	return "Press a keyboard shortcut on the current page, e.g. keys='Control+Enter' to send a message, 'Escape' to dismiss a dialog, 'Tab' to move on. The keystroke goes to the page over the protocol, so it works regardless of which window the user has focused — unlike the desktop key tools, which type wherever the machine's focus happens to be."
}
func (t *keysTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"keys": map[string]any{"type": "string", "description": "One key with optional modifiers, e.g. 'Enter', 'Escape', 'Control+Enter', 'Shift+Tab'"},
		},
		"required": []any{"keys"},
	}
}

func (t *keysTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	chord := strings.TrimSpace(tools.StringArg(args, "keys"))
	mods, key, code, vk, err := parseChord(chord)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if err := t.s.run(ctx, 20*time.Second, chromedp.ActionFunc(func(c context.Context) error {
		return pressChord(c, mods, key, code, vk)
	})); err != nil {
		return tools.Errorf("pressing %s failed: %v", chord, err)
	}
	time.Sleep(700 * time.Millisecond) // let whatever it triggered happen
	r, err := t.s.readSettled(ctx)
	if err != nil {
		return tools.Textf("Pressed %s. The page could not be re-read afterwards: %v", chord, err)
	}
	return tools.Text(fmt.Sprintf("Pressed %s.\n\n%s", chord, formatRead(r)))
}

// pressChord sends the down/up pair. A printable key with no modifier beyond
// shift also needs its char event, or the page sees the key but no text.
func pressChord(ctx context.Context, mods input.Modifier, key, code string, vk int64) error {
	down := &input.DispatchKeyEventParams{
		Type:                  input.KeyDown,
		Modifiers:             mods,
		Key:                   key,
		Code:                  code,
		WindowsVirtualKeyCode: vk,
		NativeVirtualKeyCode:  vk,
	}
	if len([]rune(key)) == 1 && mods&(input.ModifierCtrl|input.ModifierCommand|input.ModifierAlt) == 0 {
		down.Text = key
		down.UnmodifiedText = key
	}
	if err := down.Do(ctx); err != nil {
		return err
	}
	up := *down
	up.Type = input.KeyUp
	up.Text, up.UnmodifiedText = "", ""
	return up.Do(ctx)
}
