//go:build windows

package gateway

import (
	"context"
	"errors"
)

// notifyReload does nothing on Windows: there is no SIGHUP to carry a
// restart request from one process to another. A gateway upgraded from a
// terminal has to be restarted through its service manager.
func notifyReload(context.Context, func(string)) {}

// SignalRestart cannot reach another process on Windows, so the caller falls
// back to telling the user to restart the gateway.
func SignalRestart(int) error {
	return errors.New("windows cannot signal a running gateway")
}
