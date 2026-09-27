//go:build windows

package media

import (
	"context"
	"errors"
	"net"
)

// mpv speaks its IPC over a named pipe on Windows, which Go's net package
// cannot dial without cgo-free winio; until that lands the player is absent
// there and the tool says so.
func ipcAddress(string) string { return "" }

func dialIPC(context.Context, string) (net.Conn, error) {
	return nil, errors.New("the media player is not available on Windows yet")
}

func removeIPC(string) {}
