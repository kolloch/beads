package daemon

import "errors"

var (
	// ErrNotRunning is returned by client helpers when no daemon answers on the
	// socket. Callers use it to fall back to direct (non-daemon) execution.
	ErrNotRunning = errors.New("bd daemon is not running")

	// errAlreadyListening is returned by socketListen when a live daemon is
	// already bound to the socket. Surfaced by `bd daemon start` as a no-op.
	errAlreadyListening = errors.New("a bd daemon is already listening on this socket")

	// errUnsupportedPlatform is returned on platforms without Unix-domain
	// socket support (Windows).
	errUnsupportedPlatform = errors.New("bd daemon mode is only supported on Unix-like systems")
)
