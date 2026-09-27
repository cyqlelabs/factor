//go:build !windows

package media

import "net"

func listenIPC(address string) (net.Listener, error) { return net.Listen("unix", address) }
