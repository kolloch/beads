package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestCommandIntent_String(t *testing.T) {
	cases := map[CommandIntent]string{
		IntentReadOnly: "READ_ONLY",
		IntentMutating: "MUTATING",
		CommandIntent(42): "UNKNOWN",
	}
	for intent, want := range cases {
		if got := intent.String(); got != want {
			t.Errorf("CommandIntent(%d).String() = %q, want %q", intent, got, want)
		}
	}
}

func TestCommandIntentByName_ReadOnlyDefaults(t *testing.T) {
	// All entries in the read-only set must classify as READ_ONLY. This is the
	// dispatcher-level contract: store-open, post-run, and per-feature gates
	// (auto-import, auto-export, tip-metadata commit) all rely on this.
	readOnly := []string{
		"list", "ready", "show", "stats", "blocked", "count",
		"search", "query", "graph", "duplicates", "comments", "current",
		"ping", "backup", "export", "context",
	}
	for _, name := range readOnly {
		if got := commandIntentByName(name); got != IntentReadOnly {
			t.Errorf("commandIntentByName(%q) = %v, want IntentReadOnly", name, got)
		}
		if !isReadOnlyCommand(name) {
			t.Errorf("isReadOnlyCommand(%q) = false, want true", name)
		}
	}
}

func TestCommandIntentByName_MutatingDefaults(t *testing.T) {
	// Mutating commands must NOT be flagged as read-only; this is what gates
	// auto-commit, auto-push, and auto-export.
	mutating := []string{
		"create", "update", "close", "reopen", "import",
		"assign", "label", "comment", "batch", "duplicate",
		"unknown_command_name",
	}
	for _, name := range mutating {
		if got := commandIntentByName(name); got != IntentMutating {
			t.Errorf("commandIntentByName(%q) = %v, want IntentMutating", name, got)
		}
		if isReadOnlyCommand(name) {
			t.Errorf("isReadOnlyCommand(%q) = true, want false", name)
		}
	}
}

func TestCommandIntentFor_NilCommand(t *testing.T) {
	if got := commandIntentFor(nil); got != IntentMutating {
		t.Errorf("commandIntentFor(nil) = %v, want IntentMutating", got)
	}
}

func TestCommandIntentFor_DispatchesByName(t *testing.T) {
	roCmd := &cobra.Command{Use: "list"}
	if got := commandIntentFor(roCmd); got != IntentReadOnly {
		t.Errorf("commandIntentFor(list) = %v, want IntentReadOnly", got)
	}
	rwCmd := &cobra.Command{Use: "create"}
	if got := commandIntentFor(rwCmd); got != IntentMutating {
		t.Errorf("commandIntentFor(create) = %v, want IntentMutating", got)
	}
}

// TestReadOnlyCommands_MatchesContextCmd ensures the static map covers
// "context", which was previously injected by context_cmd.go init().
func TestReadOnlyCommands_MatchesContextCmd(t *testing.T) {
	if !readOnlyCommands["context"] {
		t.Fatal("'context' must be classified READ_ONLY in the static map")
	}
}

// TestQueryClassifiedReadOnly is a regression guard for ga-n2nh: `bd query` is
// a pure read (SearchIssues + label/dependency/comment lookups, the direct
// sibling of `search` and `list`). It MUST classify as read-only so the store
// opens with cfg.ReadOnly=true and skips syncCLIRemotesToSQL, which shells out
// to `dolt remote -v` (~0.9s cold dolt CLI) on remote-backed DBs. Misclassifying
// query as mutating reintroduces that per-invocation cost on every gc path that
// hits the wisp tier (e.g. gc mail inbox/check/count).
func TestQueryClassifiedReadOnly(t *testing.T) {
	if !readOnlyCommands["query"] {
		t.Fatal("'query' must be classified READ_ONLY in the static map (ga-n2nh)")
	}
	if got := commandIntentByName("query"); got != IntentReadOnly {
		t.Errorf("commandIntentByName(%q) = %v, want IntentReadOnly", "query", got)
	}
	if !isReadOnlyCommand("query") {
		t.Errorf("isReadOnlyCommand(%q) = false, want true", "query")
	}
}
