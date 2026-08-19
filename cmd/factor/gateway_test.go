package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/gateway"
)

func TestRunGatewayDaemonizedReportsPidAndLog(t *testing.T) {
	testHome(t)
	old := daemonize
	var gotPath string
	var gotArgs []string
	daemonize = func(configPath string, passthrough []string) (int, error) {
		gotPath, gotArgs = configPath, passthrough
		return 4242, nil
	}
	t.Cleanup(func() { daemonize = old })

	out, err := captureStdout(t, func() error {
		return runGateway("custom.json", true, []string{"-p", "127.0.0.1:9090"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "custom.json" {
		t.Errorf("config path %q did not reach the daemonizer", gotPath)
	}
	// A detached gateway is a fresh process: a proxy that does not travel
	// with it is a capture that quietly records nothing.
	if strings.Join(gotArgs, " ") != "-p 127.0.0.1:9090" {
		t.Errorf("proxy flags reached the daemonizer as %q", gotArgs)
	}
	if !strings.Contains(out, "4242") || !strings.Contains(out, gateway.LogPath()) {
		t.Errorf("output %q is missing the pid or the log path", out)
	}
}

// swapGatewaySeams isolates runGateway from the real daemon and the real
// tray, restoring both after the test.
func swapGatewaySeams(t *testing.T) {
	t.Helper()
	oldRun, oldTray, oldQuit := gatewayRun, trayRun, trayQuit
	t.Cleanup(func() { gatewayRun, trayRun, trayQuit = oldRun, oldTray, oldQuit })
}

func TestRunGatewayTakesTheTrayDownWithIt(t *testing.T) {
	swapGatewaySeams(t)
	quit := make(chan struct{})
	trayQuit = func() { close(quit) }
	trayRun = func(string, func() []string, func()) { <-quit } // blocks like the real loop
	gatewayRun = func(string) error { return errors.New("gateway ended") }

	if err := runGateway("", false, nil); err == nil || err.Error() != "gateway ended" {
		t.Errorf("runGateway = %v, want the gateway's error", err)
	}
	select {
	case <-quit:
	default:
		t.Error("the gateway ended but the tray was not told to quit")
	}
}

func TestRunGatewayWithoutATrayJustWaits(t *testing.T) {
	// A headless session: trayRun comes straight back, and runGateway must
	// still wait out the gateway rather than return before it.
	swapGatewaySeams(t)
	trayRun = func(string, func() []string, func()) {}
	trayQuit = func() {}
	served := false
	gatewayRun = func(string) error { served = true; return nil }

	if err := runGateway("", false, nil); err != nil {
		t.Errorf("runGateway = %v", err)
	}
	if !served {
		t.Error("runGateway returned without running the gateway")
	}
}

func TestRunGatewayDaemonizedSurfacesFailure(t *testing.T) {
	testHome(t)
	old := daemonize
	daemonize = func(string, []string) (int, error) { return 0, errors.New("no dice") }
	t.Cleanup(func() { daemonize = old })

	if err := runGateway("", true, nil); err == nil || !strings.Contains(err.Error(), "no dice") {
		t.Errorf("runGateway = %v, want the daemonizer's error", err)
	}
}
