package desktop

// Acting on a control the accessibility tree listed, rather than on a spot
// on the glass. The difference is what makes it deterministic: the control
// is found again and checked against what was listed before anything is sent,
// so a dialog that closed or a list that scrolled is a refusal rather than a
// click on whatever moved under the old coordinates; the application performs
// the action itself where it offers one, so the pointer the user may be
// holding is left where it is; and what the control holds afterwards is read
// back from the application, so the result says what happened rather than
// what was attempted.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

// focusBound bounds bringing a window forward before pointer input. It is a
// courtesy to the click, not a step it depends on: a desktop with no window
// manager has nothing to ask.
const focusBound = time.Second

const elementArgDoc = "A control from the latest screen_view's accessibility list, e.g. e3 (or f7:e3 to say which look it came from). " +
	"It is checked against the live window before anything is sent, and refused if it has gone or changed"

// elementPointer is how the mouse tool's pointer actions are spelled for the
// controller: button and click count.
var elementPointer = map[string]struct {
	button string
	count  int
}{
	"click": {"left", 1}, "double_click": {"left", 2}, "right_click": {"right", 1},
	"middle_click": {"middle", 1}, "scroll_up": {"scroll_up", 1}, "scroll_down": {"scroll_down", 1},
}

// actOnElement is the mouse tool on a listed control.
func (d *deps) actOnElement(ctx context.Context, action, ref string) *tools.Result {
	el, tree, err := d.vision.forCtx(ctx).element(ref)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if d.access == nil {
		return tools.Errorf("there is no accessibility backend on this machine; point with cell= instead")
	}
	label := el.Ref + " " + el.Label()
	if el.Disabled && action != "move" {
		return tools.Errorf("%s is disabled, so nothing was sent; it may enable once something else on the form is filled in", label)
	}
	if action == "click" && el.Can(ActPress) {
		out, err := d.access.Do(ctx, el, Act{Kind: ActPress})
		switch {
		case err == nil:
			return tools.Text("Pressed " + label + " through the application's accessibility interface; the pointer was not moved." +
				afterNote(el, out) + " screen_view to see the rest of the result.")
		case errors.Is(err, ErrStale):
			return staleResult(label, err)
		case !errors.Is(err, ErrRefused):
			return tools.Errorf("pressing %s: %v — run screen_view before trying again, so a press that did land is not repeated", label, err)
		}
		// Refused: nothing was delivered, so the pointer may try.
	}
	return d.pointerOnElement(ctx, action, el, tree, label)
}

// pointerOnElement does with the pointer what the application would not do
// itself, at the control's position as the application reports it now.
func (d *deps) pointerOnElement(ctx context.Context, action string, el Element, tree *Tree, label string) *tools.Result {
	fresh, err := d.access.Refresh(ctx, el)
	if errors.Is(err, ErrStale) {
		return staleResult(label, err)
	}
	if err != nil {
		return tools.Errorf("could not confirm %s is still there (%v); nothing was sent", label, err)
	}
	if !tree.Positioned || !fresh.HasBounds() {
		return tools.Errorf("%s has no position on screen this session can use (Wayland does not tell an application where its window is), and the application offers no direct action for %s; nothing was sent. Point at it with the grid instead", label, action)
	}
	if tree.WindowID != "" {
		// Pointer input lands on whatever window is on top at that spot, so
		// the control's own window is brought forward first.
		fctx, cancel := context.WithTimeout(ctx, focusBound)
		_ = d.ctl.Focus(fctx, Window{ID: tree.WindowID})
		cancel()
	}
	p := fresh.Center()
	if action == "move" {
		if err := d.ctl.MoveMouse(ctx, p.X, p.Y); err != nil {
			return tools.Errorf("%v", err)
		}
		return tools.Textf("Moved the pointer onto %s at %d,%d.", label, p.X, p.Y)
	}
	how, ok := elementPointer[action]
	if !ok {
		return tools.Errorf("unknown action %q", action)
	}
	if err := d.ctl.Click(ctx, how.button, how.count, &Point{X: p.X, Y: p.Y}); err != nil {
		return tools.Errorf("%v", err)
	}
	why := ""
	if action == "click" {
		why = " (the application offers no direct action for it, so the pointer did it)"
	}
	return tools.Textf("%s on %s at %d,%d, the center of its outline as the application reports it now%s. The screen may have changed — screen_view to verify the result.",
		action, label, p.X, p.Y, why)
}

// staleResult is the refusal for a control that is not the one listed.
func staleResult(label string, err error) *tools.Result {
	detail := ""
	if msg := err.Error(); msg != ErrStale.Error() {
		detail = " (" + strings.TrimPrefix(msg, ErrStale.Error()+": ") + ")"
	}
	return tools.Errorf("%s is no longer there%s — the window changed since screen_view, so nothing was sent. Run screen_view again and use the control it lists now", label, detail)
}

// afterNote says what a press changed about the control itself.
func afterNote(was Element, out Outcome) string {
	if out.Gone {
		return " It is gone now: the surface it was on closed, which is what pressing it usually means."
	}
	now := out.After
	var parts []string
	if now.Checked != was.Checked && now.Checked != TriNone {
		parts = append(parts, map[Tri]string{TriOn: "checked", TriOff: "unchecked"}[now.Checked])
	}
	if now.Expanded != was.Expanded && now.Expanded != TriNone {
		parts = append(parts, map[Tri]string{TriOn: "expanded", TriOff: "collapsed"}[now.Expanded])
	}
	if now.Selected && !was.Selected {
		parts = append(parts, "selected")
	}
	if len(parts) == 0 {
		return ""
	}
	return " It is now " + strings.Join(parts, " and ") + "."
}

// typeIntoElement replaces a listed field's contents with text.
func (d *deps) typeIntoElement(ctx context.Context, ref, text string, delay int) *tools.Result {
	el, tree, err := d.vision.forCtx(ctx).element(ref)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if d.access == nil {
		return tools.Errorf("there is no accessibility backend on this machine; focus the field and type_text without element")
	}
	label := el.Ref + " " + el.Label()
	if el.Disabled || el.ReadOnly {
		return tools.Errorf("%s does not accept text (it is %s), so nothing was sent", label, map[bool]string{true: "disabled", false: "read-only"}[el.Disabled])
	}
	if el.Can(ActSetText) {
		out, err := d.access.Do(ctx, el, Act{Kind: ActSetText, Text: text})
		switch {
		case err == nil:
			return tools.Text("Set " + label + " directly through the application's accessibility interface; no key was pressed." + readBack(el, out, text))
		case errors.Is(err, ErrStale):
			return staleResult(label, err)
		case !errors.Is(err, ErrRefused):
			return tools.Errorf("writing into %s: %v — run screen_view to see what the field holds before trying again", label, err)
		}
	}
	// Keystrokes: the field is focused, its contents selected, and the text
	// typed over them — which is what replacing a field's value by hand is.
	if r, ok := controlRune(text); ok {
		return tools.Errorf("%s does not accept a direct write, and text containing %s cannot be typed safely: a keystroke sends it as a key. Type each line separately and press_key between them", label, runeName(r))
	}
	if el.Can(ActFocus) {
		if _, err := d.access.Do(ctx, el, Act{Kind: ActFocus}); errors.Is(err, ErrStale) {
			return staleResult(label, err)
		} else if err != nil {
			if res := d.pointerOnElement(ctx, "click", el, tree, label); res.IsError {
				return res
			}
		}
	} else if res := d.pointerOnElement(ctx, "click", el, tree, label); res.IsError {
		return res
	}
	selectAll := "ctrl+a"
	if d.env.GOOS == "darwin" {
		selectAll = "cmd+a"
	}
	if err := d.ctl.PressKey(ctx, selectAll, 1); err != nil {
		return tools.Errorf("focused %s but could not select its contents: %v", label, err)
	}
	if err := d.ctl.TypeText(ctx, text, delay); err != nil {
		return tools.Errorf("%v", err)
	}
	msg := "Typed into " + label + " by keystroke (the application does not accept a direct write), over its selected contents."
	if now, err := d.access.Refresh(ctx, el); err == nil {
		msg += readBack(el, Outcome{After: now}, text)
	}
	return tools.Text(msg)
}

// readBack says what the field holds after a write, as the application reads
// it back — the only evidence that the text landed, and landed whole.
func readBack(was Element, out Outcome, text string) string {
	switch {
	case was.Secret:
		return fmt.Sprintf(" It is a secret field, so its %d characters were not read back.", len([]rune(text)))
	case out.Gone:
		return " The field is gone now, so it could not be read back."
	case out.After.Value == text:
		return " Read back from the application: it holds exactly that."
	case strings.TrimSpace(out.After.Value) == strings.TrimSpace(text):
		return " Read back from the application: it holds that, give or take surrounding spaces."
	case out.After.Value == "" && was.Value == "":
		return " The application does not report the field's contents, so it could not be read back: screen_view to check."
	}
	return fmt.Sprintf(" But the application now holds %q, not what was written: it may format, limit or reject what it accepts.", clipValue(out.After.Value))
}
