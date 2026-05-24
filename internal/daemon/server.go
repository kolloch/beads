package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// QueryStore is the subset of storage operations the daemon serves. Narrowing
// to these read-only methods keeps the daemon's contract explicit (it cannot
// mutate) and makes the server testable with a lightweight fake instead of the
// full DoltStorage surface. A real *storage.DoltStorage satisfies it.
type QueryStore interface {
	GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error)
	GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error)
	GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error)
	GetStatistics(ctx context.Context) (*types.Statistics, error)
}

// Options configures a Server.
type Options struct {
	// Version is the bd version string reported by OpPing.
	Version string
	// IdleTimeout, when > 0, shuts the daemon down after it has had no active
	// connections and no requests for the given duration. Zero disables idle
	// shutdown (the daemon runs until signaled or asked to stop).
	IdleTimeout time.Duration
}

// Server serves read-only bd queries over a connection, reusing a single
// held storage connection across every request. It is safe for concurrent
// connections: all served ops are reads, and the underlying *sql.DB-backed
// store is concurrency-safe.
type Server struct {
	store   QueryStore
	version string

	idleTimeout time.Duration

	mu           sync.Mutex
	activeConns  int
	lastActivity time.Time

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
}

// NewServer creates a Server backed by an already-open (read-only) store. The
// caller retains ownership of store and is responsible for closing it after
// Serve returns.
func NewServer(store QueryStore, opts Options) *Server {
	return &Server{
		store:        store,
		version:      opts.Version,
		idleTimeout:  opts.IdleTimeout,
		lastActivity: time.Now(),
		shutdownCh:   make(chan struct{}),
	}
}

// ListenAndServe binds the daemon socket at socketPath and serves until ctx is
// canceled, a shutdown request arrives, or the idle timeout expires. It removes
// the socket file on return. Returns errAlreadyListening if a live daemon is
// already bound.
func (s *Server) ListenAndServe(ctx context.Context, socketPath string) error {
	ln, err := Listen(socketPath)
	if err != nil {
		return err
	}
	// Best-effort removal of our own socket on exit; Serve closes the listener.
	defer func() { _ = os.Remove(socketPath) }()
	return s.Serve(ctx, ln)
}

// Serve runs the accept loop on ln. It always closes ln before returning, and
// waits for in-flight connection handlers to drain. Serve takes ownership of
// ln's lifecycle but not the store's.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	var (
		wg        sync.WaitGroup
		closeOnce sync.Once
	)
	closeListener := func() { closeOnce.Do(func() { _ = ln.Close() }) }

	// Watcher goroutine: translate cancellation, an explicit shutdown request,
	// and idle expiry into a single listener close, which unblocks Accept.
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		var idleC <-chan time.Time
		if s.idleTimeout > 0 {
			t := time.NewTicker(idleCheckInterval(s.idleTimeout))
			defer t.Stop()
			idleC = t.C
		}
		for {
			select {
			case <-ctx.Done():
				closeListener()
				return
			case <-s.shutdownCh:
				closeListener()
				return
			case <-idleC:
				if s.idleExpired() {
					s.triggerShutdown()
				}
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Accept fails when the listener is closed (the shutdown path) — or
			// on a transient error. Distinguish: if we're shutting down, stop;
			// otherwise keep serving.
			if s.isShuttingDown(ctx) {
				break
			}
			if errors.Is(err, net.ErrClosed) {
				break
			}
			continue
		}
		s.connOpened()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.connClosed()
			s.handleConn(ctx, conn)
		}()
	}

	wg.Wait()
	s.triggerShutdown() // ensure the watcher exits even on Accept-error break
	<-watcherDone
	return nil
}

// Shutdown asks a running Serve loop to stop. Safe to call multiple times and
// from any goroutine.
func (s *Server) Shutdown() { s.triggerShutdown() }

func (s *Server) triggerShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdownCh) })
}

func (s *Server) isShuttingDown(ctx context.Context) bool {
	select {
	case <-s.shutdownCh:
		return true
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func (s *Server) connOpened() {
	s.mu.Lock()
	s.activeConns++
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *Server) connClosed() {
	s.mu.Lock()
	s.activeConns--
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *Server) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *Server) idleExpired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeConns == 0 && time.Since(s.lastActivity) >= s.idleTimeout
}

// handleConn serves requests on a single connection until the peer closes it.
// Reusing one connection for many requests is what avoids per-call setup cost.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		var req Request
		if err := readMessage(conn, &req); err != nil {
			// io.EOF (clean close) or any read error ends this connection.
			return
		}
		s.touch()
		resp := s.dispatch(ctx, &req)
		if err := writeMessage(conn, &resp); err != nil {
			return
		}
		if req.Op == OpShutdown && resp.OK {
			s.triggerShutdown()
			return
		}
	}
}

// dispatch executes a single request against the held store and returns the
// response. It never panics out of the daemon: store errors become error
// responses, not process exits.
func (s *Server) dispatch(ctx context.Context, req *Request) Response {
	if req.Version != 0 && req.Version != ProtocolVersion {
		return errResponse(fmt.Sprintf("unsupported protocol version %d (daemon speaks %d)", req.Version, ProtocolVersion))
	}

	switch req.Op {
	case OpPing:
		return okResponse(PingResult{
			Pong:            true,
			Pid:             os.Getpid(),
			Version:         s.version,
			ProtocolVersion: ProtocolVersion,
		})

	case OpReady:
		var p ReadyParams
		if err := decodeParams(req.Params, &p); err != nil {
			return errResponse(err.Error())
		}
		issues, err := s.store.GetReadyWork(ctx, p.Filter)
		if err != nil {
			return errResponse(err.Error())
		}
		return okResponse(issues)

	case OpBlocked:
		var p BlockedParams
		if err := decodeParams(req.Params, &p); err != nil {
			return errResponse(err.Error())
		}
		blocked, err := s.store.GetBlockedIssues(ctx, p.Filter)
		if err != nil {
			return errResponse(err.Error())
		}
		return okResponse(blocked)

	case OpShow:
		var p ShowParams
		if err := decodeParams(req.Params, &p); err != nil {
			return errResponse(err.Error())
		}
		if len(p.IDs) == 0 {
			return errResponse("show: no issue IDs provided")
		}
		issues, err := s.store.GetIssuesByIDs(ctx, p.IDs)
		if err != nil {
			return errResponse(err.Error())
		}
		return okResponse(issues)

	case OpStats:
		stats, err := s.store.GetStatistics(ctx)
		if err != nil {
			return errResponse(err.Error())
		}
		return okResponse(stats)

	case OpShutdown:
		// The actual teardown happens in handleConn after the reply is sent, so
		// the client observes a clean OK before the socket goes away.
		return okResponse(map[string]bool{"shutting_down": true})

	default:
		return errResponse(fmt.Sprintf("unknown op %q", req.Op))
	}
}

// decodeParams unmarshals op params. Empty params are valid for ops whose
// payload is optional (e.g. ready/blocked with a zero-value filter).
func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode params: %w", err)
	}
	return nil
}

func okResponse(result any) Response {
	body, err := json.Marshal(result)
	if err != nil {
		return errResponse(fmt.Sprintf("marshal result: %v", err))
	}
	return Response{OK: true, Result: body}
}

func errResponse(msg string) Response {
	return Response{OK: false, Error: msg}
}

// idleCheckInterval picks how often to poll for idle expiry: frequently enough
// to react promptly relative to the timeout, but bounded so a long timeout does
// not spin. Clamped to [20ms, 2s].
func idleCheckInterval(idleTimeout time.Duration) time.Duration {
	interval := idleTimeout / 4
	if interval < 20*time.Millisecond {
		interval = 20 * time.Millisecond
	}
	if interval > 2*time.Second {
		interval = 2 * time.Second
	}
	return interval
}
