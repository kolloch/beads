package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/daemon"
	"github.com/steveyegge/beads/internal/storage/dbproxy/pidfile"
)

// Daemon state files live alongside the workspace's other beads state in the
// resolved .beads directory.
const (
	daemonPidFileName = "daemon.pid"
	daemonLogFileName = "daemon.log"
)

var (
	daemonForeground  bool
	daemonIdleTimeout time.Duration
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run or control the bd query daemon (reuses one Dolt connection)",
	Long: `Run a long-lived daemon that holds a single read-only Dolt connection open
and serves the hot read-only query commands (ready, show, stats, blocked) over
a Unix-domain socket.

Under heavy agent polling, each cold ` + "`bd <subcommand>`" + ` pays a fresh Dolt
connection: an auth handshake plus server-side per-connection thread setup. The
daemon amortizes that across every request by reusing one held connection,
collapsing steady-state connection churn from dozens/sec to ~1.

The daemon serves only read-only operations. Mutating commands continue to run
directly; see docs/DAEMON.md for the socket contract and the deferred work.

  bd daemon start              # start detached
  bd daemon start --foreground # run in the foreground (logs to stderr)
  bd daemon status             # is a daemon running for this workspace?
  bd daemon ping               # round-trip a ping and print latency
  bd daemon stop               # ask the daemon to shut down`,
	// The daemon manages its own storage lifecycle; skip the root store-open /
	// auto-commit / close pipeline, which is built for one-shot CLI invocations
	// and would open (and close) a connection the daemon must instead hold.
	PersistentPreRun:  func(cmd *cobra.Command, args []string) {},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {},
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the bd query daemon (detached unless --foreground)",
	RunE: func(cmd *cobra.Command, args []string) error {
		beadsDir, socketPath, err := resolveDaemonTarget()
		if err != nil {
			return err
		}
		if daemon.IsRunning(socketPath) {
			fmt.Printf("bd daemon already running for %s (socket %s)\n", beadsDir, socketPath)
			return nil
		}
		if daemonForeground {
			return runDaemonForeground(beadsDir, socketPath)
		}
		return startDaemonDetached(beadsDir, socketPath)
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report whether a bd daemon is running for this workspace",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, socketPath, err := resolveDaemonTarget()
		if err != nil {
			return err
		}
		if !daemon.IsRunning(socketPath) {
			fmt.Printf("bd daemon: not running (socket %s)\n", socketPath)
			return nil
		}
		c, err := daemon.Dial(socketPath, 2*time.Second)
		if err != nil {
			fmt.Printf("bd daemon: not running (socket %s)\n", socketPath)
			return nil
		}
		defer func() { _ = c.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pong, err := c.Ping(ctx)
		if err != nil {
			return fmt.Errorf("daemon did not respond: %w", err)
		}
		fmt.Printf("bd daemon: running (pid %d, version %s, protocol v%d, socket %s)\n",
			pong.Pid, pong.Version, pong.ProtocolVersion, socketPath)
		return nil
	},
}

var daemonPingCmd = &cobra.Command{
	Use:   "ping",
	Short: "Round-trip a ping to the daemon and print latency",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, socketPath, err := resolveDaemonTarget()
		if err != nil {
			return err
		}
		c, err := daemon.Dial(socketPath, 2*time.Second)
		if err != nil {
			return fmt.Errorf("%w (socket %s)", err, socketPath)
		}
		defer func() { _ = c.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		pong, err := c.Ping(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("pong from pid %d in %s\n", pong.Pid, time.Since(start).Round(time.Microsecond))
		return nil
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Ask the running bd daemon to shut down",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, socketPath, err := resolveDaemonTarget()
		if err != nil {
			return err
		}
		if !daemon.IsRunning(socketPath) {
			fmt.Printf("bd daemon: not running (socket %s)\n", socketPath)
			return nil
		}
		c, err := daemon.Dial(socketPath, 2*time.Second)
		if err != nil {
			return fmt.Errorf("%w (socket %s)", err, socketPath)
		}
		defer func() { _ = c.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown request failed: %w", err)
		}
		fmt.Println("bd daemon: shutdown requested")
		return nil
	},
}

// resolveDaemonTarget resolves the workspace .beads directory and its daemon
// socket path. It honors -C (like the rest of bd) and otherwise discovers the
// workspace from BEADS_DIR / the working directory.
func resolveDaemonTarget() (beadsDir, socketPath string, err error) {
	if changeDir != "" {
		beadsDir, err = resolveChangeDirBeadsDir(changeDir)
		if err != nil {
			return "", "", err
		}
	} else {
		beadsDir = beads.FindBeadsDir()
	}
	if beadsDir == "" {
		return "", "", fmt.Errorf("no beads workspace found; run inside a beads project or set BEADS_DIR")
	}
	socketPath, err = daemon.DefaultSocketPath(beadsDir)
	if err != nil {
		return "", "", err
	}
	return beadsDir, socketPath, nil
}

// runDaemonForeground opens the held read-only store and serves until signaled,
// asked to stop, or idle past --idle-timeout.
func runDaemonForeground(beadsDir, socketPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	store, err := newReadOnlyStoreFromConfig(ctx, beadsDir)
	if err != nil {
		return fmt.Errorf("open read-only store: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := pidfile.Write(beadsDir, daemonPidFileName, pidfile.PidFile{Pid: os.Getpid()}); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}
	defer func() { _ = pidfile.Remove(beadsDir, daemonPidFileName) }()

	srv := daemon.NewServer(store, daemon.Options{Version: Version, IdleTimeout: daemonIdleTimeout})
	fmt.Fprintf(os.Stderr, "bd daemon listening on %s (pid %d)\n", socketPath, os.Getpid())
	if err := srv.ListenAndServe(ctx, socketPath); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	fmt.Fprintln(os.Stderr, "bd daemon stopped")
	return nil
}

// startDaemonDetached forks `bd daemon start --foreground` as a detached
// process, then waits for it to become ready. The child inherits the resolved
// workspace via BEADS_DIR so it serves the same database.
func startDaemonDetached(beadsDir, socketPath string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate bd executable: %w", err)
	}

	args := []string{"daemon", "start", "--foreground"}
	if daemonIdleTimeout > 0 {
		args = append(args, "--idle-timeout", daemonIdleTimeout.String())
	}

	logPath := filepath.Join(beadsDir, daemonLogFileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: logPath is workspace-derived, not user-request input
	if err != nil {
		return fmt.Errorf("open daemon log %q: %w", logPath, err)
	}

	child := exec.Command(self, args...)
	child.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	child.SysProcAttr = daemonProcAttrDetached()

	if err := child.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start detached daemon: %w", err)
	}
	// The child holds its own copy of the log fd; the parent can close ours.
	_ = logFile.Close()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.IsRunning(socketPath) {
			fmt.Printf("bd daemon started (pid %d, socket %s)\n", child.Process.Pid, socketPath)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready within 10s; see %s", logPath)
}

func init() {
	daemonStartCmd.Flags().BoolVar(&daemonForeground, "foreground", false, "run in the foreground instead of detaching")
	daemonStartCmd.Flags().DurationVar(&daemonIdleTimeout, "idle-timeout", 0, "shut down after this long with no activity (0 = run until stopped)")
	daemonCmd.AddCommand(daemonStartCmd, daemonStatusCmd, daemonPingCmd, daemonStopCmd)
	rootCmd.AddCommand(daemonCmd)
}
