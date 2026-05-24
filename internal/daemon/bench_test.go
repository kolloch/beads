//go:build !windows

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// The benchmarks below isolate the IPC layer that the daemon adds. They do not
// (and cannot, in-process) reproduce the full cold-`bd` cost — fork+exec plus a
// fresh Dolt auth handshake and server-side per-connection thread setup — which
// is what the be-3t4 acceptance ("100× sequential 5-10× faster") measures
// end-to-end. See docs/DAEMON.md for the shell-level benchmark recipe.
//
// What these prove is the part the daemon controls: a warm, reused connection
// serves a request in a single socket round-trip, materially cheaper than even
// re-establishing the lightweight Unix-domain connection each call — and far
// cheaper than the per-call Dolt connection the daemon eliminates.

func benchServer(b *testing.B) string {
	b.Helper()
	store := &fakeStore{ready: []*types.Issue{{ID: "be-1", Title: "bench"}}}
	socketPath, err := DefaultSocketPath(b.TempDir())
	if err != nil {
		b.Fatalf("DefaultSocketPath: %v", err)
	}
	ln, err := Listen(socketPath)
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	srv := NewServer(store, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx, ln) }()
	b.Cleanup(cancel)
	for i := 0; i < 200; i++ {
		if IsRunning(socketPath) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return socketPath
}

// BenchmarkReadyWarmConn measures per-request latency on a single reused
// connection — the steady-state path under ambient polling.
func BenchmarkReadyWarmConn(b *testing.B) {
	socketPath := benchServer(b)
	c, err := Dial(socketPath, 2*time.Second)
	if err != nil {
		b.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Ready(ctx, types.WorkFilter{}); err != nil {
			b.Fatalf("Ready: %v", err)
		}
	}
}

// BenchmarkReadyColdConn measures the cost of establishing a fresh connection
// per request. The delta over BenchmarkReadyWarmConn is the connection-setup
// cost that reuse removes (and is itself far below the Dolt-connection cost the
// daemon eliminates relative to cold `bd`).
func BenchmarkReadyColdConn(b *testing.B) {
	socketPath := benchServer(b)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := Dial(socketPath, 2*time.Second)
		if err != nil {
			b.Fatalf("Dial: %v", err)
		}
		if _, err := c.Ready(ctx, types.WorkFilter{}); err != nil {
			b.Fatalf("Ready: %v", err)
		}
		_ = c.Close()
	}
}
