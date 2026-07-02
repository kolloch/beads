package main

import "github.com/spf13/cobra"

// CommandIntent describes whether a bd subcommand reads from or writes to the
// database. This is the dispatcher-level single source of truth used by the
// store-open path, the post-run commit/push pipeline, and per-feature gates
// (auto-import, auto-export, auto-backup, tip metadata commits).
//
// Behavior contract (Layer 1):
//
//   IntentReadOnly:
//     - Opens the dolt store with cfg.ReadOnly = true (schema init skipped).
//     - PersistentPostRun does NOT issue DOLT_COMMIT for tip metadata.
//       Pending tip writes accumulate in the working set and ride along on
//       the next mutating command's commit.
//     - Skips auto-push and post-run auto-export.
//
//   IntentMutating:
//     - Opens the store in read-write mode (schema init performed when needed).
//     - PersistentPostRun runs the normal auto-commit / auto-push pipeline,
//       gated on commandDidWrite.
//     - Tip metadata commits run as before.
//
// Commands that can be either depending on flags (e.g. `bd ready --claim`)
// remain classified as IntentReadOnly here. The actual mutating code path
// sets commandDidWrite, which triggers the normal auto-commit pipeline; the
// dispatcher classification only governs *defaults* and the read-only-only
// fast paths.
type CommandIntent int

const (
	// IntentMutating is the safe default: assume a command can write.
	IntentMutating CommandIntent = iota
	// IntentReadOnly marks a command as not normally writing to the database.
	IntentReadOnly
)

// String renders the intent for log messages and debug output.
func (i CommandIntent) String() string {
	switch i {
	case IntentReadOnly:
		return "READ_ONLY"
	case IntentMutating:
		return "MUTATING"
	default:
		return "UNKNOWN"
	}
}

// readOnlyCommands lists commands that only read from the database.
// These commands open the store in read-only mode (cfg.ReadOnly=true) to
// avoid modifying the database (which breaks file watchers, contends on
// dolt locks under concurrent reads, and adds noise to dolt history).
//
// See GH#804 for the original read-only opt-in; the CommandIntent enum
// is the dispatcher-level wrapper introduced for Layer 1.
//
// Note: bd ready is classified read-only because the common path is the
// no-flag read. The --claim path writes, but routes through the normal
// commandDidWrite/auto-commit pipeline; the dispatcher classification only
// affects defaults and the tip-metadata-commit fast path.
var readOnlyCommands = map[string]bool{
	"list":       true,
	"ready":      true,
	"show":       true,
	"stats":      true,
	"blocked":    true,
	"count":      true,
	"search":     true,
	"query":      true, // pure read (SearchIssues); sibling of search — skips the dolt remote -v probe (ga-n2nh)
	"graph":      true,
	"duplicates": true,
	"comments":   true, // list comments (not add)
	"current":    true, // bd sync mode current
	"ping":       true,
	"backup":     true, // reads from Dolt, writes only to .beads/backup/
	"export":     true, // reads from Dolt, writes JSONL to file/stdout
	"context":    true, // diagnostics only; no DB writes
}

// isReadOnlyCommand returns true if the named top-level subcommand is
// classified as read-only at the dispatcher. Kept as a thin wrapper over
// commandIntentByName so existing call sites compile unchanged.
func isReadOnlyCommand(cmdName string) bool {
	return commandIntentByName(cmdName) == IntentReadOnly
}

// commandIntentByName returns the intent for a top-level bd subcommand name.
// Unknown commands default to IntentMutating (safe default).
func commandIntentByName(name string) CommandIntent {
	if readOnlyCommands[name] {
		return IntentReadOnly
	}
	return IntentMutating
}

// commandIntentFor returns the intent for the given Cobra command. Returns
// IntentMutating when cmd is nil (safe default).
func commandIntentFor(cmd *cobra.Command) CommandIntent {
	if cmd == nil {
		return IntentMutating
	}
	return commandIntentByName(cmd.Name())
}
