package desktop

// Guards on synthesized keyboard input. Both exist for the same reason the
// path guard does: they stop an accident, not an attacker. A personal agent
// works on the user's own session, and a few keystrokes there do far more
// than the turn meant — they lock the screen, switch the window everything
// after them lands in, or kill the graphical session outright.

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// blockedCombos are the shortcuts that act on the session rather than on the
// window in front. Each one moves the user somewhere the turn did not mean to
// go, and every keystroke after it lands there instead. The list follows the
// one agent-desktop refuses by default; application shortcuts such as ctrl+q
// or ctrl+w are deliberately absent, since closing a document is something a
// user asks for and the window says so before it loses anything.
var blockedCombos = func() map[string]string {
	m := map[string]string{
		"alt+f4":             "closes the window in front",
		"alt+tab":            "switches to another window",
		"alt+shift+tab":      "switches to another window",
		"alt+f2":             "opens the desktop's run dialog",
		"super":              "opens the desktop's launcher",
		"super+l":            "locks the screen",
		"super+d":            "hides every window",
		"super+tab":          "switches to another window",
		"ctrl+alt+t":         "opens a terminal",
		"ctrl+alt+delete":    "opens the session menu",
		"ctrl+alt+backspace": "kills the graphical session",
		"ctrl+alt+l":         "locks the screen",
	}
	for i := 1; i <= 12; i++ {
		m[fmt.Sprintf("ctrl+alt+f%d", i)] = "switches to another virtual terminal"
	}
	return m
}()

// keyAliases folds the many names one key goes by into the one the
// blocklist is written in.
var keyAliases = map[string]string{
	"control": "ctrl", "ctl": "ctrl", "control_l": "ctrl", "control_r": "ctrl",
	"option": "alt", "alt_l": "alt", "alt_r": "alt", "meta": "super",
	"cmd": "super", "command": "super", "win": "super", "windows": "super", "logo": "super",
	"super_l": "super", "super_r": "super", "shift_l": "shift", "shift_r": "shift",
	"del": "delete", "esc": "escape", "iso_left_tab": "tab",
}

// modifierRank orders modifiers the way the blocklist spells them.
var modifierRank = map[string]int{"ctrl": 0, "alt": 1, "shift": 2, "super": 3}

// canonicalCombo spells one combination the way the blocklist does: lower
// case, aliases folded, modifiers in a fixed order, so "Alt+Tab",
// "tab+alt" and "Option+Tab" are the same question.
func canonicalCombo(combo string) string {
	var mods, keys []string
	for _, part := range strings.Split(combo, "+") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "" {
			continue
		}
		if a, ok := keyAliases[p]; ok {
			p = a
		}
		if _, ok := modifierRank[p]; ok {
			mods = append(mods, p)
			continue
		}
		keys = append(keys, p)
	}
	sort.Slice(mods, func(i, j int) bool { return modifierRank[mods[i]] < modifierRank[mods[j]] })
	// A lone modifier is its own key: "super" pressed on its own opens the
	// launcher, which is exactly the press the list exists for.
	return strings.Join(append(mods, keys...), "+")
}

// blockedCombo reports why a key sequence would be refused, or "" when it is
// an ordinary press. xdotool takes several combinations separated by spaces,
// so each one is checked.
func blockedCombo(keys string) (combo, why string) {
	for _, c := range strings.Fields(keys) {
		canon := canonicalCombo(c)
		if why, ok := blockedCombos[canon]; ok {
			return c, why
		}
	}
	return "", ""
}

// controlRune finds the first character a keystroke would turn into a key
// rather than a letter: a newline is Return, which submits the form it lands
// in, and a tab moves focus to the next field, so every character after it is
// typed somewhere else. Text written through an application's own interface
// carries them safely; keystrokes do not.
func controlRune(text string) (rune, bool) {
	for _, r := range text {
		if unicode.IsControl(r) {
			return r, true
		}
	}
	return 0, false
}

// runeName names a control character the way a person would read it.
func runeName(r rune) string {
	switch r {
	case '\n':
		return "a newline"
	case '\r':
		return "a carriage return"
	case '\t':
		return "a tab"
	}
	return fmt.Sprintf("the control character %U", r)
}
