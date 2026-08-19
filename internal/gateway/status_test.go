package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestStatusLinesNameWhatRunsAndWhatDoesNot(t *testing.T) {
	lines := statusLines("v1.2.3", 3*time.Minute, true, true, []string{"telegram", "voice"})
	for i, want := range []string{"factor v1.2.3 — up 3m", "memory: healthy", "channels: telegram, voice"} {
		if lines[i] != want {
			t.Errorf("line %d = %q, want %q", i, lines[i], want)
		}
	}

	lines = statusLines("dev", time.Second, false, false, nil)
	if lines[1] != "memory: off" || lines[2] != "channels: none" {
		t.Errorf("bare gateway lines = %q", lines)
	}

	// Healthy is only worth reporting for a memory that is on; an unhealthy
	// one must say so rather than hide behind the version row.
	if lines := statusLines("dev", time.Second, true, false, nil); lines[1] != "memory: unreachable" {
		t.Errorf("sick memory reported as %q", lines[1])
	}
}

func TestUpWordsPicksAGlanceablePrecision(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{20 * time.Second, "moments"},
		{5 * time.Minute, "5m"},
		{3*time.Hour + 12*time.Minute, "3h 12m"},
		{50 * time.Hour, "2d 2h"},
	} {
		if got := upWords(tc.d); got != tc.want {
			t.Errorf("upWords(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestStatusLinesFallBackWhileNothingServes(t *testing.T) {
	if lines := StatusLines(); len(lines) != 1 || !strings.Contains(lines[0], "starting") {
		t.Errorf("no gateway in this process, yet StatusLines = %q", lines)
	}
}
