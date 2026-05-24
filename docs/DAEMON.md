# bd daemon mode (Unix-socket query daemon)

`bd daemon` runs a long-lived process that holds **one** read-only Dolt
connection open and serves the hot read-only query commands over a
Unix-domain socket. It is Layer 2 of the dolt-connection-reuse effort
(`be-3t4`; Layer 1 was the dispatcher-level `READ_ONLY` classification in
`cmd/bd/intent.go`, `be-y9p`).

## Why

Under heavy agent polling, every cold `bd <subcommand>` pays for a fresh Dolt
connection: a MySQL auth handshake (~1–3 ms) plus server-side per-connection
thread setup, on top of process fork+exec. With dozens of agents polling
`bd ready` / `bd show` / mailbox state, the Dolt sql-server sees 60+ new
connections per second, and its thread pool becomes the bottleneck (HQ
`pe-4lpn`).

The daemon amortizes that cost: it opens the connection once and reuses it for
every request, collapsing steady-state connection churn to ~1. A reused socket
connection answers a query in a single round-trip (microseconds), versus the
multi-millisecond cold path.

A Unix socket (not loopback TCP) is used purely to sidestep TLS / firewall /
port-allocation friction — the RTT difference is negligible.

## Commands

```bash
bd daemon start                 # start detached (logs to .beads/daemon.log)
bd daemon start --foreground    # run in the foreground; logs to stderr
bd daemon start --idle-timeout 10m   # auto-stop after 10m with no activity
bd daemon status                # is a daemon running for this workspace?
bd daemon ping                  # round-trip a ping; prints latency
bd daemon stop                  # ask the daemon to shut down gracefully
```

The workspace is resolved the usual way (`-C`, then `BEADS_DIR`, then the
working directory). Daemon state lives alongside the workspace's other beads
state in the resolved `.beads` directory:

| File | Purpose |
|------|---------|
| `daemon.sock` | the Unix-domain socket (removed on shutdown) |
| `daemon.pid`  | the daemon process id (removed on shutdown) |
| `daemon.log`  | stdout/stderr of a detached daemon |

If `<beadsDir>/daemon.sock` would exceed the platform's `sun_path` limit (deep
worktrees), the socket falls back to a stable hashed path under the OS temp
directory, keyed on the absolute beads dir.

Daemon mode requires a Unix-like OS; on Windows the commands return an
"unsupported" error.

## Wire contract

The socket speaks a **length-prefixed JSON** protocol. Every message is:

```
[4-byte big-endian uint32 length N][N bytes of UTF-8 JSON]
```

A client sends a `Request`; the daemon replies with exactly one `Response`.
Multiple request/response pairs may be pipelined over a single connection —
reusing the connection is what eliminates per-call connection setup. The daemon
reads requests in a loop until the peer closes the connection. Maximum framed
message size is 64 MiB.

This contract is process- and language-agnostic on purpose: the gascity `gc`
sidecar (a separate binary) connects to the same socket and speaks the same
framing without importing bd. The Go reference client is
`internal/daemon.Client`.

### Request

```json
{ "v": 1, "op": "ready", "params": { "filter": { "limit": 10 } } }
```

| Field    | Type   | Notes |
|----------|--------|-------|
| `v`      | int    | protocol version the client speaks (current: `1`). `0` is treated as "unversioned" and accepted. |
| `op`     | string | the operation (see below). |
| `params` | object | op-specific payload; omitted/empty for ops that take none. |

### Response

```json
{ "ok": true, "result": [ /* op-specific JSON */ ] }
{ "ok": false, "error": "issue not found: be-999" }
```

### Operations (Layer 2a — read-only)

| `op` | params | result |
|------|--------|--------|
| `ping`    | — | `{ "pong": true, "pid": 1234, "version": "1.0.4", "protocol_version": 1 }` |
| `ready`   | `{ "filter": WorkFilter }` | `[]Issue` (same semantics as `bd ready`) |
| `blocked` | `{ "filter": WorkFilter }` | `[]BlockedIssue` |
| `show`    | `{ "ids": ["be-1","be-2"] }` | `[]Issue` (batched lookup) |
| `stats`   | — | `Statistics` |
| `shutdown`| — | `{ "shutting_down": true }`, then the daemon exits |

`WorkFilter`, `Issue`, `BlockedIssue`, and `Statistics` are the JSON encodings
of the corresponding `internal/types` structs — the *caller* builds the filter
(assignee, labels, limit, sort), so the daemon applies identical blocker-aware
semantics to its single held connection.

The daemon serves **only read-only operations**. A store error becomes an error
*response* (`ok:false`), never a process exit — the daemon stays up.

## Consuming the daemon (gc sidecar)

The intended consumer is a `gc` sidecar that routes the hot read-only
`gc bd` / `gc mail` polling commands through the socket and falls back to a
direct `bd` fork+exec for everything else:

1. Resolve the socket path for the workspace (`<beadsDir>/daemon.sock`, with the
   temp-dir fallback above) — or call `bd daemon status`.
2. If a daemon answers (`ping`), map the supported read commands to `op`s and
   round-trip them on a reused connection.
3. Otherwise (or for any unsupported/mutating command), run `bd` directly.

`internal/daemon.IsRunning(socketPath)` and `internal/daemon.Dial(...)` are the
Go helpers for this decision.

## Benchmarking the win (acceptance)

The `be-3t4` acceptance is "`bd <subcommand>` 100× sequential via the daemon
completes 5–10× faster than today." The Go benchmarks
(`internal/daemon`, `BenchmarkReadyWarmConn` / `BenchmarkReadyColdConn`) isolate
the IPC layer; the end-to-end win is dominated by eliminating fork+exec + the
per-call Dolt connection. A shell-level comparison:

```bash
# Today: cold bd, fresh Dolt connection each call.
time (for i in $(seq 100); do bd ready --json >/dev/null; done)

# With the daemon: a persistent client reuses one connection. Drive it from
# the sidecar, or from a small client that dials once and sends 100 requests.
bd daemon start
# ... 100 ready round-trips over one socket connection ...
bd daemon stop
```

Watch Dolt's connection count (e.g. `SHOW STATUS LIKE 'Threads_connected'`)
under ambient agent polling: it should drop from dozens/sec of new connections
to a single held connection.

## Deferred (follow-up)

Layer 2a deliberately scopes to the read-only ambient-polling path. Two pieces
are tracked as follow-up work because they require a larger, riskier change —
running arbitrary cobra command logic in-process inside the daemon is blocked by
the ~233 `os.Exit` call sites and direct-`os.Stdout` writes across `cmd/bd`,
which are unsafe in a long-lived server:

- **Generic subcommand execution** (serving arbitrary `bd`/`gc bd` subcommands
  over the socket), which needs `FatalError`/`os.Exit` converted to a
  recoverable in-daemon error path and stdout/stderr capture.
- **Write-commit batching** (serving mutating commands and batching their Dolt
  commits).

Transparent rerouting of the real `bd` CLI through the daemon is also out of
scope here — the sidecar is the consumer. See the follow-up bead linked from
`be-3t4`.
