//go:build !windows

package media

import (
	"context"
	"net"
	"os"
	"path/filepath"
)

// ipcAddress is where mpv listens for this process: a socket under Factor's
// home, or under the temp dir where the home path is too long for one.
func ipcAddress(home string) string {
	path := filepath.Join(home, "media.sock")
	if len(path) >= 100 { // sun_path is 108 bytes on Linux, 104 on macOS
		path = filepath.Join(os.TempDir(), "factor-media.sock")
	}
	return path
}

func dialIPC(ctx context.Context, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", address)
}

func removeIPC(address string) { _ = os.Remove(address) }
