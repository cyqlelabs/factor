//go:build !windows

package media

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// ipcAddress is where mpv listens for this process: a socket under Factor's
// home, or under the temp dir where the home path is too long for one. The
// name carries the pid so a gateway and a terminal session on one machine
// each drive their own player rather than the first one's.
func ipcAddress(home string) string {
	name := fmt.Sprintf("media-%d.sock", os.Getpid())
	path := filepath.Join(home, name)
	if len(path) >= 100 { // sun_path is 108 bytes on Linux, 104 on macOS
		path = filepath.Join(os.TempDir(), "factor-"+name)
	}
	return path
}

func dialIPC(ctx context.Context, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", address)
}

func removeIPC(address string) { _ = os.Remove(address) }
