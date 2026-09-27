//go:build windows

package media

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/Microsoft/go-winio"
)

// mpv speaks its IPC over a named pipe on Windows. The name carries the pid
// so a gateway and a terminal session on one machine each drive their own
// player rather than the first one's.
func ipcAddress(string) string {
	return fmt.Sprintf(`\\.\pipe\factor-media-%d`, os.Getpid())
}

// dialIPC opens the pipe with overlapped I/O, so the reader goroutine's
// pending read does not block the writes the same connection carries.
func dialIPC(ctx context.Context, address string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, address)
}

// A named pipe goes away with its last handle; there is nothing to unlink.
func removeIPC(string) {}
