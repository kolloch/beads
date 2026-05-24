package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// Client is a connection to a running bd daemon. A single Client holds one
// socket connection and pipelines many request/response round-trips over it —
// reusing the connection (and the daemon's held Dolt connection behind it) is
// what eliminates the per-invocation auth handshake and connection setup that
// dominate cold `bd <subcommand>` latency.
//
// A Client is not safe for concurrent use by multiple goroutines; open one
// Client per goroutine.
type Client struct {
	conn net.Conn
}

// Dial connects to the daemon listening at socketPath. The returned Client owns
// the connection; call Close when finished. If no daemon answers, the error
// wraps ErrNotRunning so callers can fall back to direct execution.
func Dial(socketPath string, timeout time.Duration) (*Client, error) {
	conn, err := socketDial(socketPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	return &Client{conn: conn}, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// call performs one request/response round-trip. If ctx carries a deadline it
// is applied to the socket for the duration of the call.
func (c *Client) call(ctx context.Context, op Op, params any, out any) error {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(dl)
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}

	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		raw = b
	}

	req := Request{Version: ProtocolVersion, Op: op, Params: raw}
	if err := writeMessage(c.conn, &req); err != nil {
		return fmt.Errorf("send %s request: %w", op, err)
	}

	var resp Response
	if err := readMessage(c.conn, &resp); err != nil {
		return fmt.Errorf("read %s response: %w", op, err)
	}
	if !resp.OK {
		return fmt.Errorf("daemon %s error: %s", op, resp.Error)
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", op, err)
		}
	}
	return nil
}

// Ping probes the daemon and returns its identity.
func (c *Client) Ping(ctx context.Context) (PingResult, error) {
	var r PingResult
	err := c.call(ctx, OpPing, nil, &r)
	return r, err
}

// Ready returns claimable work matching filter (same semantics as `bd ready`).
func (c *Client) Ready(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error) {
	var r []*types.Issue
	err := c.call(ctx, OpReady, ReadyParams{Filter: filter}, &r)
	return r, err
}

// Blocked returns blocked issues matching filter (same semantics as `bd blocked`).
func (c *Client) Blocked(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	var r []*types.BlockedIssue
	err := c.call(ctx, OpBlocked, BlockedParams{Filter: filter}, &r)
	return r, err
}

// Show returns the issues for the given IDs in one batched lookup.
func (c *Client) Show(ctx context.Context, ids []string) ([]*types.Issue, error) {
	var r []*types.Issue
	err := c.call(ctx, OpShow, ShowParams{IDs: ids}, &r)
	return r, err
}

// Stats returns project statistics (same as `bd stats`).
func (c *Client) Stats(ctx context.Context) (*types.Statistics, error) {
	var r types.Statistics
	err := c.call(ctx, OpStats, nil, &r)
	return &r, err
}

// Shutdown asks the daemon to stop. The daemon replies before tearing down.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, OpShutdown, nil, nil)
}

// IsRunning reports whether a live daemon answers on socketPath. It is a
// cheap connect+ping with a short timeout, suitable for the client-routing
// decision and `bd daemon status`.
func IsRunning(socketPath string) bool {
	c, err := Dial(socketPath, 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = c.Ping(ctx)
	return err == nil
}
