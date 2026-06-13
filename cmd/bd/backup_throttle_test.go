package main

import (
	"testing"
	"time"
)

// TestTouchBackupThrottle verifies that persisting the throttle after a failed
// auto-backup bumps the timestamp (so the next mutation is throttled) while
// preserving the change-detection watermark (so the backup is retried after the
// interval, not abandoned). See be-lee.
func TestTouchBackupThrottle(t *testing.T) {
	t.Run("creates state when none exists", func(t *testing.T) {
		dir := t.TempDir()
		touchBackupThrottle(dir)

		state, err := loadBackupState(dir)
		if err != nil {
			t.Fatalf("loadBackupState: %v", err)
		}
		if state.Timestamp.IsZero() {
			t.Error("expected throttle timestamp to be set")
		}
		if state.LastDoltCommit != "" {
			t.Errorf("expected empty commit watermark, got %q", state.LastDoltCommit)
		}
	})

	t.Run("preserves commit watermark and advances timestamp", func(t *testing.T) {
		dir := t.TempDir()
		old := time.Now().UTC().Add(-time.Hour)
		if err := saveBackupState(dir, &backupState{LastDoltCommit: "abc123", Timestamp: old}); err != nil {
			t.Fatalf("saveBackupState: %v", err)
		}

		touchBackupThrottle(dir)

		state, err := loadBackupState(dir)
		if err != nil {
			t.Fatalf("loadBackupState: %v", err)
		}
		if state.LastDoltCommit != "abc123" {
			t.Errorf("commit watermark not preserved: got %q, want %q", state.LastDoltCommit, "abc123")
		}
		if !state.Timestamp.After(old) {
			t.Errorf("expected timestamp to advance past %v, got %v", old, state.Timestamp)
		}
	})
}
