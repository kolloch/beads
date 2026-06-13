package versioncontrolops

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// fakeBackupConn is a DBConn whose ExecContext returns a fixed error, used to
// exercise the error-classification logic in the idempotent backup helpers.
// Only ExecContext is exercised by BackupAdd/BackupRemove.
type fakeBackupConn struct {
	execErr error
	execN   int
}

func (f *fakeBackupConn) ExecContext(_ context.Context, _ string, _ ...any) (sql.Result, error) {
	f.execN++
	return nil, f.execErr
}

func (f *fakeBackupConn) QueryContext(_ context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return nil, nil
}

func (f *fakeBackupConn) QueryRowContext(_ context.Context, _ string, _ ...any) *sql.Row {
	return nil
}

func TestBackupAddIfNotExists(t *testing.T) {
	tests := []struct {
		name    string
		execErr error
		wantErr bool
	}{
		{"clean add succeeds", nil, false},
		{"already exists is swallowed", fmt.Errorf("backup 'backup_export' already exists"), false},
		// An address conflict means a *different* remote holds the URL, so the
		// requested name was NOT created — it must propagate so the caller can
		// sync via the conflicting remote instead of this absent one.
		{"address conflict propagates", fmt.Errorf("address conflict with a remote: 'default' -> file:///b"), true},
		{"unrelated error propagates", fmt.Errorf("connection refused"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &fakeBackupConn{execErr: tt.execErr}
			err := BackupAddIfNotExists(context.Background(), conn, "backup_export", "file:///b")
			if (err != nil) != tt.wantErr {
				t.Fatalf("BackupAddIfNotExists() error = %v, wantErr %v", err, tt.wantErr)
			}
			if conn.execN != 1 {
				t.Errorf("expected exactly one ExecContext call, got %d", conn.execN)
			}
		})
	}
}

func TestBackupRemoveIfExists(t *testing.T) {
	tests := []struct {
		name    string
		execErr error
		wantErr bool
	}{
		{"clean remove succeeds", nil, false},
		{"not found is swallowed", fmt.Errorf("backup 'backup_export' not found"), false},
		{"unrelated error propagates", fmt.Errorf("connection refused"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &fakeBackupConn{execErr: tt.execErr}
			err := BackupRemoveIfExists(context.Background(), conn, "backup_export")
			if (err != nil) != tt.wantErr {
				t.Fatalf("BackupRemoveIfExists() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBackupErrorPredicates(t *testing.T) {
	// Critical invariant: an address conflict must NOT be classified as
	// "already exists", or BackupAddIfNotExists would swallow it and the
	// sync-via-conflicting-remote path would never run.
	conflict := fmt.Errorf("add backup backup_export: address conflict with a remote: 'default' -> file:///b")
	if isBackupAlreadyExists(conflict) {
		t.Error("address conflict must not be treated as already-exists")
	}
	if !isBackupAlreadyExists(fmt.Errorf("backup 'x' already exists")) {
		t.Error("expected already-exists to be detected")
	}
	if !isBackupNotFound(fmt.Errorf("backup 'x' not found")) {
		t.Error("expected not-found to be detected")
	}
	if isBackupAlreadyExists(nil) || isBackupNotFound(nil) {
		t.Error("nil error must not match any predicate")
	}
}

func TestExtractAddressConflictName(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
		{
			name: "unrelated error",
			err:  fmt.Errorf("connection refused"),
			want: "",
		},
		{
			name: "standard conflict",
			err:  fmt.Errorf("Error 1105: address conflict with a remote: 'default' -> file:///backup"),
			want: "default",
		},
		{
			name: "full dolt error format from doc comment",
			err:  fmt.Errorf("Error 1105: address conflict with a remote: 'backup_export' -> file:///some/path"),
			want: "backup_export",
		},
		{
			name: "missing closing quote",
			err:  fmt.Errorf("address conflict with a remote: 'oops"),
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractAddressConflictName(tt.err); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
