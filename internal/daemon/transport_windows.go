//go:build windows

package daemon

import (
	"net"
	"time"
)

// DefaultSocketPath is unsupported on Windows: bd daemon mode requires
// Unix-domain sockets.
func DefaultSocketPath(string) (string, error) {
	return "", errUnsupportedPlatform
}

// Listen is unsupported on Windows.
func Listen(string) (net.Listener, error) {
	return nil, errUnsupportedPlatform
}

// socketDial is unsupported on Windows.
func socketDial(string, time.Duration) (net.Conn, error) {
	return nil, errUnsupportedPlatform
}

// socketSupported reports whether daemon mode is available on this platform.
func socketSupported() bool { return false }
