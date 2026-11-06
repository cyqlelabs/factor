//go:build !linux

package desktop

// newPlatformAccessor has no AT-SPI to speak to off Linux; Windows and macOS
// are answered by their own backends in NewAccessor.
func newPlatformAccessor(Env, Controller) Accessor { return nil }
