// Package daemon implements bd's long-lived daemon mode (Layer 2 of the
// dolt-connection-reuse effort, be-3t4). The daemon holds a single read-only
// storage connection open and serves the hot ambient-polling read commands
// (ready / show / stats / blocked) over a Unix-domain socket, so callers no
// longer pay the per-invocation auth handshake and dolt-side per-connection
// thread setup that dominate `bd <subcommand>` latency under heavy agent
// polling.
//
// # Wire contract
//
// The socket speaks a length-prefixed JSON protocol. Every message is:
//
//	[4 bytes big-endian uint32 length N][N bytes of JSON]
//
// A client sends a Request and the server replies with exactly one Response.
// Multiple request/response pairs may be pipelined over a single connection;
// reusing the connection is what eliminates per-call connection setup. The
// server reads requests in a loop until the peer closes the connection.
//
// This contract is intentionally process- and language-agnostic: the gascity
// `gc` sidecar (a separate binary) connects to the same socket and speaks the
// same framing without importing this package. See docs/DAEMON.md.
package daemon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/steveyegge/beads/internal/types"
)

// ProtocolVersion identifies the wire contract. Bump on breaking changes so a
// mismatched client/daemon pair can fail loudly instead of misparsing.
const ProtocolVersion = 1

// maxMessageBytes caps a single framed message. Issue lists can be large, but a
// bound prevents a corrupt/hostile length prefix from triggering an unbounded
// allocation.
const maxMessageBytes = 64 << 20 // 64 MiB

// Op names a daemon operation. Layer 2a ships the read-only query ops that
// dominate ambient agent polling; mutating/generic-subcommand ops are a
// deliberate follow-up (see docs/DAEMON.md, "Deferred").
type Op string

const (
	// OpPing is a liveness/handshake probe. Params are ignored; the result is a
	// PingResult carrying the daemon's pid and version.
	OpPing Op = "ping"
	// OpReady returns claimable work. Params: ReadyParams.
	OpReady Op = "ready"
	// OpShow returns issues by ID. Params: ShowParams.
	OpShow Op = "show"
	// OpStats returns project statistics. Params: none.
	OpStats Op = "stats"
	// OpBlocked returns blocked issues. Params: BlockedParams.
	OpBlocked Op = "blocked"
	// OpShutdown asks the daemon to shut down gracefully. The daemon replies OK
	// before tearing down. Used by `bd daemon stop`.
	OpShutdown Op = "shutdown"
)

// Request is the client→daemon message.
type Request struct {
	// Version is the protocol version the client speaks. The daemon rejects a
	// request whose major contract it cannot honor.
	Version int `json:"v"`
	// Op selects the operation.
	Op Op `json:"op"`
	// Params is the op-specific payload (see the *Params types). May be empty
	// for ops that take no parameters (ping, stats, shutdown).
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the daemon→client message. Exactly one is sent per Request.
type Response struct {
	// OK reports whether the op succeeded. When false, Error is populated and
	// Result is empty.
	OK bool `json:"ok"`
	// Result is the op-specific JSON payload on success.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is a human-readable message on failure.
	Error string `json:"error,omitempty"`
}

// ReadyParams is the payload for OpReady. Filter mirrors the WorkFilter the
// `bd ready` command builds, so the daemon applies identical blocker-aware
// semantics — the caller, not the daemon, decides assignee/label/limit scope.
type ReadyParams struct {
	Filter types.WorkFilter `json:"filter"`
}

// BlockedParams is the payload for OpBlocked.
type BlockedParams struct {
	Filter types.WorkFilter `json:"filter"`
}

// ShowParams is the payload for OpShow. IDs are resolved in one batched lookup.
type ShowParams struct {
	IDs []string `json:"ids"`
}

// PingResult is the payload returned by OpPing.
type PingResult struct {
	Pong            bool   `json:"pong"`
	Pid             int    `json:"pid"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
}

// writeMessage frames v as length-prefixed JSON and writes it to w.
func writeMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	if len(body) > maxMessageBytes {
		return fmt.Errorf("message too large: %d bytes (max %d)", len(body), maxMessageBytes)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("write length prefix: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

// readMessage reads one length-prefixed JSON message from r into v. It returns
// io.EOF when the peer closes the connection cleanly between messages, which
// callers use as the normal end-of-stream signal.
func readMessage(r io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		// io.EOF here means a clean close at a message boundary; surface it
		// verbatim so the read loop can distinguish it from a truncated frame.
		if err == io.EOF {
			return io.EOF
		}
		return fmt.Errorf("read length prefix: %w", err)
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > maxMessageBytes {
		return fmt.Errorf("framed message too large: %d bytes (max %d)", n, maxMessageBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		// A partial frame after the header is a protocol error, not a clean EOF.
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return io.ErrUnexpectedEOF
		}
		return fmt.Errorf("read body: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("unmarshal message: %w", err)
	}
	return nil
}
