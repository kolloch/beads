//go:build !windows

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// maxUnixSocketPath is a conservative cap on the Unix-domain sun_path length.
// Linux allows 108 and macOS/BSD 104; staying under the smallest keeps socket
// paths portable across the platforms bd targets.
const maxUnixSocketPath = 100

// DefaultSocketPath returns the daemon socket path for a beads directory. The
// natural location is <beadsDir>/daemon.sock, colocated with the other daemon
// state (daemon.pid, daemon.log). Unix-domain socket paths are length-limited,
// so when the natural path is too long (deep polecat worktrees) fall back to a
// stable hashed name under the OS temp dir — the hash is derived from the
// absolute beads dir, so the same workspace always maps to the same socket.
func DefaultSocketPath(beadsDir string) (string, error) {
	if beadsDir == "" {
		return "", fmt.Errorf("beads directory is required to resolve the daemon socket path")
	}
	abs, err := filepath.Abs(beadsDir)
	if err != nil {
		return "", fmt.Errorf("resolve beads dir %q: %w", beadsDir, err)
	}
	natural := filepath.Join(abs, "daemon.sock")
	if len(natural) <= maxUnixSocketPath {
		return natural, nil
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(os.TempDir(), "bd-daemon-"+hex.EncodeToString(sum[:8])+".sock"), nil
}

// Listen binds a Unix-domain listener at socketPath. A leftover socket file
// from a crashed daemon is removed first, but only after confirming nothing is
// currently listening — so a live daemon is never yanked out from under itself.
// Returns errAlreadyListening when a live listener is detected; callers treat
// that as "already running".
func Listen(socketPath string) (net.Listener, error) {
	if _, err := os.Stat(socketPath); err == nil {
		// A socket file exists. If a connect succeeds, a daemon is live and we
		// must not disturb it.
		if conn, derr := net.DialTimeout("unix", socketPath, 250*time.Millisecond); derr == nil {
			_ = conn.Close()
			return nil, errAlreadyListening
		}
		// Nothing answered: a stale socket from a crashed daemon. Safe to clear.
		if rerr := os.Remove(socketPath); rerr != nil {
			return nil, fmt.Errorf("remove stale socket %q: %w", socketPath, rerr)
		}
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %q: %w", socketPath, err)
	}
	// Issue data is owner-private; restrict the socket to the owner.
	if cerr := os.Chmod(socketPath, 0o600); cerr != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket %q: %w", socketPath, cerr)
	}
	return ln, nil
}

// socketDial connects to a daemon socket with a dial timeout.
func socketDial(socketPath string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", socketPath, timeout)
}

// socketSupported reports whether daemon mode is available on this platform.
func socketSupported() bool { return true }
