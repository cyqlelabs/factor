package gateway

import "testing"

// requireSighup skips a test whose trigger is a signal this platform cannot
// deliver. The reload those tests drive is covered portably by
// TestRunReloadsWhenTheConfigFileChanges, which asks through the config file.
func requireSighup(t *testing.T) {
	t.Helper()
	if !sighupSupported {
		t.Skip("no SIGHUP on this platform; the config-change reload covers this path")
	}
}
