//go:build windows

package media

import (
	"net"

	"github.com/Microsoft/go-winio"
)

func listenIPC(address string) (net.Listener, error) { return winio.ListenPipe(address, nil) }
