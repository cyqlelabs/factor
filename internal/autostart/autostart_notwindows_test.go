//go:build !windows

package autostart

import (
	"context"
	"testing"
)

// Off Windows the GOOS dispatch still has to reach the registry entry points;
// what it finds there is the stub, which must fail rather than claim success.
func TestWindowsDispatchReachesTheRegistryStub(t *testing.T) {
	env, _, _ := fakeEnv(t, "windows", "")
	if _, err := Install(context.Background(), env, `C:\factor.exe`, ""); err == nil {
		t.Error("the non-windows registry stub reported success")
	}
	if _, ok := Installed(env); ok {
		t.Error("Installed = true through the non-windows stub")
	}
	if err := Uninstall(context.Background(), env); err == nil {
		t.Error("Uninstall through the stub reported success")
	}
}
