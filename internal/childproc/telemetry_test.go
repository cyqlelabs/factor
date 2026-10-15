package childproc

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// A child started on Quiet sees every switch, including one the parent's
// environment had set the other way.
func TestQuietOverridesWhatTheEnvironmentHeld(t *testing.T) {
	if os.Getenv("FACTOR_CHILDPROC_PRINT_ENV") == "1" {
		for _, kv := range NoTelemetry {
			k, _, _ := strings.Cut(kv, "=")
			fmt.Println(k + "=" + os.Getenv(k))
		}
		os.Exit(0)
	}
	base := append(os.Environ(), "DO_NOT_TRACK=0", "SENTRY_DSN=https://key@sentry.example/1",
		"FACTOR_CHILDPROC_PRINT_ENV=1")
	cmd := exec.Command(os.Args[0], "-test.run=^TestQuietOverridesWhatTheEnvironmentHeld$")
	cmd.Env = Quiet(base)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(out)), "\n")
	if !slices.Equal(got, NoTelemetry) {
		t.Errorf("the child saw %q, want %q", got, NoTelemetry)
	}
}
