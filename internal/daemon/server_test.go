//go:build !windows

package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// fakeStore is a minimal QueryStore for exercising the daemon without Dolt.
type fakeStore struct {
	mu         sync.Mutex
	ready      []*types.Issue
	blocked    []*types.BlockedIssue
	byID       map[string]*types.Issue
	stats      *types.Statistics
	readyErr   error
	calls      int
	lastFilter types.WorkFilter
}

func (f *fakeStore) GetReadyWork(_ context.Context, filter types.WorkFilter) ([]*types.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	if f.readyErr != nil {
		return nil, f.readyErr
	}
	return f.ready, nil
}

func (f *fakeStore) GetBlockedIssues(_ context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	return f.blocked, nil
}

func (f *fakeStore) GetIssuesByIDs(_ context.Context, ids []string) ([]*types.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*types.Issue
	for _, id := range ids {
		if iss, ok := f.byID[id]; ok {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (f *fakeStore) GetStatistics(_ context.Context) (*types.Statistics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, nil
}

func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestServer starts a daemon on a temp socket and returns its socket path,
// the Serve error channel, and a cancel func. Serve is canceled on cleanup.
func newTestServer(t *testing.T, store QueryStore, opts Options) (string, <-chan error, context.CancelFunc) {
	t.Helper()
	socketPath, err := DefaultSocketPath(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultSocketPath: %v", err)
	}
	ln, err := Listen(socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := NewServer(store, opts)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx, ln) }()
	t.Cleanup(cancel)
	if !waitRunning(socketPath, 2*time.Second) {
		cancel()
		t.Fatal("daemon did not become ready")
	}
	return socketPath, errCh, cancel
}

func waitRunning(socketPath string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if IsRunning(socketPath) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func dialTest(t *testing.T, socketPath string) *Client {
	t.Helper()
	c, err := Dial(socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func bg(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPing(t *testing.T) {
	socketPath, _, _ := newTestServer(t, &fakeStore{}, Options{Version: "test-1.2.3"})
	c := dialTest(t, socketPath)
	pong, err := c.Ping(bg(t))
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !pong.Pong || pong.Version != "test-1.2.3" || pong.ProtocolVersion != ProtocolVersion {
		t.Fatalf("unexpected ping result: %+v", pong)
	}
}

func TestReadyPassesFilterAndReturnsIssues(t *testing.T) {
	store := &fakeStore{ready: []*types.Issue{{ID: "be-1", Title: "first"}, {ID: "be-2", Title: "second"}}}
	socketPath, _, _ := newTestServer(t, store, Options{})
	c := dialTest(t, socketPath)

	assignee := "gastown.furiosa"
	filter := types.WorkFilter{Assignee: &assignee, Limit: 7}
	got, err := c.Ready(bg(t), filter)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 2 || got[0].ID != "be-1" || got[1].ID != "be-2" {
		t.Fatalf("unexpected ready issues: %+v", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lastFilter.Assignee == nil || *store.lastFilter.Assignee != assignee || store.lastFilter.Limit != 7 {
		t.Fatalf("filter not passed through: %+v", store.lastFilter)
	}
}

func TestShow(t *testing.T) {
	store := &fakeStore{byID: map[string]*types.Issue{"be-9": {ID: "be-9", Title: "nine"}}}
	socketPath, _, _ := newTestServer(t, store, Options{})
	c := dialTest(t, socketPath)
	got, err := c.Show(bg(t), []string{"be-9", "be-missing"})
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(got) != 1 || got[0].ID != "be-9" {
		t.Fatalf("unexpected show result: %+v", got)
	}
}

func TestShowRejectsEmptyIDs(t *testing.T) {
	socketPath, _, _ := newTestServer(t, &fakeStore{}, Options{})
	c := dialTest(t, socketPath)
	if _, err := c.Show(bg(t), nil); err == nil {
		t.Fatal("expected error for empty ID list")
	}
}

func TestStats(t *testing.T) {
	store := &fakeStore{stats: &types.Statistics{TotalIssues: 42, OpenIssues: 10}}
	socketPath, _, _ := newTestServer(t, store, Options{})
	c := dialTest(t, socketPath)
	got, err := c.Stats(bg(t))
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.TotalIssues != 42 || got.OpenIssues != 10 {
		t.Fatalf("unexpected stats: %+v", got)
	}
}

func TestBlocked(t *testing.T) {
	store := &fakeStore{blocked: []*types.BlockedIssue{{Issue: types.Issue{ID: "be-b"}, BlockedByCount: 2, BlockedBy: []string{"be-x", "be-y"}}}}
	socketPath, _, _ := newTestServer(t, store, Options{})
	c := dialTest(t, socketPath)
	got, err := c.Blocked(bg(t), types.WorkFilter{})
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	if len(got) != 1 || got[0].ID != "be-b" || got[0].BlockedByCount != 2 {
		t.Fatalf("unexpected blocked result: %+v", got)
	}
}

func TestStoreErrorPropagatesAsResponseError(t *testing.T) {
	store := &fakeStore{readyErr: errors.New("boom from store")}
	socketPath, _, _ := newTestServer(t, store, Options{})
	c := dialTest(t, socketPath)
	_, err := c.Ready(bg(t), types.WorkFilter{})
	if err == nil || !contains(err.Error(), "boom from store") {
		t.Fatalf("expected store error to propagate, got %v", err)
	}
}

// TestConnectionReuse is the core Layer 2 behavior: many requests over one
// connection (and one held store), with no per-call reconnect.
func TestConnectionReuse(t *testing.T) {
	store := &fakeStore{ready: []*types.Issue{{ID: "be-1"}}}
	socketPath, _, _ := newTestServer(t, store, Options{})
	c := dialTest(t, socketPath)
	const n = 50
	for i := 0; i < n; i++ {
		if _, err := c.Ready(bg(t), types.WorkFilter{}); err != nil {
			t.Fatalf("Ready #%d: %v", i, err)
		}
	}
	if store.callCount() != n {
		t.Fatalf("expected %d store calls, got %d", n, store.callCount())
	}
}

func TestConcurrentClients(t *testing.T) {
	store := &fakeStore{ready: []*types.Issue{{ID: "be-1"}}}
	socketPath, _, _ := newTestServer(t, store, Options{})

	const clients = 16
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := Dial(socketPath, 2*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = c.Close() }()
			for j := 0; j < 5; j++ {
				if _, err := c.Ready(bg(t), types.WorkFilter{}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent client error: %v", err)
	}
	if store.callCount() != clients*5 {
		t.Fatalf("expected %d calls, got %d", clients*5, store.callCount())
	}
}

func TestUnknownOpAndVersionMismatch(t *testing.T) {
	socketPath, _, _ := newTestServer(t, &fakeStore{}, Options{})

	// Unknown op.
	conn, err := socketDial(socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := writeMessage(conn, &Request{Version: ProtocolVersion, Op: "definitely-not-an-op"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp Response
	if err := readMessage(conn, &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.OK || !contains(resp.Error, "unknown op") {
		t.Fatalf("expected unknown-op error, got %+v", resp)
	}

	// Version mismatch on the same connection.
	if err := writeMessage(conn, &Request{Version: 999, Op: OpPing}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := readMessage(conn, &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.OK || !contains(resp.Error, "unsupported protocol version") {
		t.Fatalf("expected version-mismatch error, got %+v", resp)
	}
}

func TestExplicitShutdown(t *testing.T) {
	socketPath, errCh, _ := newTestServer(t, &fakeStore{}, Options{})
	c := dialTest(t, socketPath)
	if err := c.Shutdown(bg(t)); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve returned error after shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after explicit shutdown")
	}
}

func TestIdleTimeoutShutsDown(t *testing.T) {
	store := &fakeStore{}
	socketPath, err := DefaultSocketPath(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultSocketPath: %v", err)
	}
	ln, err := Listen(socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := NewServer(store, Options{IdleTimeout: 150 * time.Millisecond})
	errCh := make(chan error, 1)
	// No pings: pinging counts as activity and would reset the idle timer.
	go func() { errCh <- srv.Serve(context.Background(), ln) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve returned error on idle shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not idle-shutdown")
	}
}

func TestContextCancelStopsServe(t *testing.T) {
	_, errCh, cancel := newTestServer(t, &fakeStore{}, Options{})
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve returned error on cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
}

func TestIsRunningFalseWhenNoDaemon(t *testing.T) {
	socketPath, err := DefaultSocketPath(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultSocketPath: %v", err)
	}
	if IsRunning(socketPath) {
		t.Fatal("IsRunning should be false when no daemon is listening")
	}
	c, err := Dial(socketPath, 200*time.Millisecond)
	if err == nil {
		_ = c.Close()
		t.Fatal("Dial should fail when no daemon is listening")
	}
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}
}

// TestStaleSocketReclaimed verifies a leftover socket file from a crashed daemon
// is reclaimed by the next Listen rather than blocking startup.
func TestStaleSocketReclaimed(t *testing.T) {
	socketPath, err := DefaultSocketPath(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultSocketPath: %v", err)
	}
	ln1, err := Listen(socketPath)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	// Simulate a crash: close the listener but leave the socket file behind.
	if l, ok := ln1.(interface{ Close() error }); ok {
		_ = l.Close()
	}
	// The file may still exist; a fresh Listen must reclaim it.
	ln2, err := Listen(socketPath)
	if err != nil {
		t.Fatalf("second Listen should reclaim stale socket, got: %v", err)
	}
	_ = ln2.Close()
}

func TestListenRejectsLiveSocket(t *testing.T) {
	socketPath, _, _ := newTestServer(t, &fakeStore{}, Options{})
	_, err := Listen(socketPath)
	if !errors.Is(err, errAlreadyListening) {
		t.Fatalf("expected errAlreadyListening for a live socket, got %v", err)
	}
}
