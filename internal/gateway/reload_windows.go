//go:build windows

package gateway

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// reloadTimeout bounds the request below. The daemon answers as soon as it has
// taken the request; the reload itself happens afterwards, once the turn in
// flight has been answered.
const reloadTimeout = 5 * time.Second

// notifyReload does nothing on Windows: there is no SIGHUP to carry a restart
// request from one process to another. The control endpoint carries it
// instead, and SignalRestart below is what knocks on it.
func notifyReload(context.Context, func(string)) {}

// SignalRestart asks the gateway to reload into the binary now on disk.
//
// Windows has no signal that reaches a process this one did not start, which
// left `factor upgrade` in a terminal installing a new binary that the running
// daemon went on ignoring — indefinitely, since nothing else would ever tell
// it, and a second upgrade then failed outright on the binary the old process
// still held. The gateway's own loopback endpoint is the door that does exist.
func SignalRestart(addr string, _ int) error { return requestReloadOver(addr) }

// requestReloadOver asks the gateway listening on addr to restart into the
// binary now on disk.
func requestReloadOver(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), reloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/reload", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("asking the gateway on %s to reload: %w", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("the gateway on %s answered %s", addr, resp.Status)
	}
	return nil
}
