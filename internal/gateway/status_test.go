package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestStatusLinesNameWhatRunsAndWhatDoesNot(t *testing.T) {
	lines := statusLines("v1.2.3", 3*time.Minute, true, true, []string{"telegram", "voice"}, nil, "spend: $0.42 today · $12.30 all-time")
	for i, want := range []string{"factor v1.2.3", "up 3m", "memory: healthy", "channels: telegram, voice", "spend: $0.42 today · $12.30 all-time"} {
		if lines[i] != want {
			t.Errorf("line %d = %q, want %q", i, lines[i], want)
		}
	}

	lines = statusLines("dev", time.Second, false, false, nil, nil, "")
	if lines[2] != "memory: off" || lines[3] != "channels: none" {
		t.Errorf("bare gateway lines = %q", lines)
	}
	// Nothing counted is not the same fact as nothing spent, so an empty
	// spend line is left out rather than shown as zero.
	if len(lines) != 4 {
		t.Errorf("unmetered gateway lines = %q, want no spend row", lines)
	}

	// Healthy is only worth reporting for a memory that is on; an unhealthy
	// one must say so rather than hide behind the version row.
	if lines := statusLines("dev", time.Second, true, false, nil, nil, ""); lines[2] != "memory: unreachable" {
		t.Errorf("sick memory reported as %q", lines[2])
	}

	// A connector that did not come up is named, and stays out of the
	// channels row: that row is what a message can be addressed to.
	lines = statusLines("dev", time.Second, true, true, []string{"telegram"},
		map[string]string{"voice": "no microphone"}, "")
	if lines[3] != "channels: telegram" || lines[4] != "not running: voice" {
		t.Errorf("failed-connector rows = %q", lines)
	}
}

func TestUpWordsPicksAGlanceablePrecision(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{20 * time.Second, "20s"},
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
