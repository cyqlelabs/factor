package config

import "testing"

// Reasoning is written out in full, empty fields included, so the three states
// the provider layer branches on have to be distinguishable from the struct
// alone: unconfigured, explicitly off, and configured.
func TestReasoningStates(t *testing.T) {
	cases := []struct {
		name      string
		r         ReasoningConfig
		zero, off bool
	}{
		{"unset", ReasoningConfig{}, true, false},
		{"explicit off", ReasoningConfig{Effort: "none"}, false, true},
		{"effort", ReasoningConfig{Effort: "high"}, false, false},
		{"budget", ReasoningConfig{MaxTokens: 2048}, false, false},
		{"off with a budget is not off", ReasoningConfig{Effort: "none", MaxTokens: 2048}, false, false},
		{"exclude alone still counts as configured", ReasoningConfig{Exclude: true}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.IsZero(); got != tc.zero {
				t.Errorf("IsZero() = %v, want %v", got, tc.zero)
			}
			if got := tc.r.Off(); got != tc.off {
				t.Errorf("Off() = %v, want %v", got, tc.off)
			}
		})
	}
}

// Desktop tools are prompt weight on a headless box, so they follow the
// display unless the user has an opinion either way.
func TestDesktopRegister(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name       string
		cfg        DesktopConfig
		hasDisplay bool
		want       bool
	}{
		{"auto with a display", DesktopConfig{}, true, true},
		{"auto headless", DesktopConfig{}, false, false},
		{"forced on while headless", DesktopConfig{Enabled: &on}, false, true},
		{"forced off with a display", DesktopConfig{Enabled: &off}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Register(tc.hasDisplay); got != tc.want {
				t.Errorf("Register(%v) = %v, want %v", tc.hasDisplay, got, tc.want)
			}
		})
	}
}
