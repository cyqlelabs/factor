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
	daemonize = func(configPath string) (int, error) {
		gotPath = configPath
		return 4242, nil
	}
	t.Cleanup(func() { daemonize = old })

	out, err := captureStdout(t, func() error { return runGateway("custom.json", true) })
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "custom.json" {
		t.Errorf("config path %q did not reach the daemonizer", gotPath)
	}
	if !strings.Contains(out, "4242") || !strings.Contains(out, gateway.LogPath()) {
		t.Errorf("output %q is missing the pid or the log path", out)
	}
}

func TestRunGatewayDaemonizedSurfacesFailure(t *testing.T) {
	testHome(t)
	old := daemonize
	daemonize = func(string) (int, error) { return 0, errors.New("no dice") }
	t.Cleanup(func() { daemonize = old })

	if err := runGateway("", true); err == nil || !strings.Contains(err.Error(), "no dice") {
		t.Errorf("runGateway = %v, want the daemonizer's error", err)
	}
}
