package versioncontrolops

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// BackupAdd registers a Dolt backup destination.
func BackupAdd(ctx context.Context, db DBConn, name, url string) error {
	if _, err := db.ExecContext(ctx, "CALL DOLT_BACKUP('add', ?, ?)", name, url); err != nil {
		return fmt.Errorf("add backup %s: %w", name, err)
	}
	return nil
}

// BackupAddIfNotExists registers a Dolt backup destination idempotently: if a
// backup with the same name is already registered, the "already exists" error
// is treated as success. This is the concurrency-safe variant for callers that
// share a Dolt sql-server with other processes, where a peer may have created
// the same global backup remote between this caller's remove and add.
//
// An *address conflict* — a different remote name already pointing at url — is
// deliberately NOT swallowed: the requested name still does not exist, so the
// caller must inspect the returned error with ExtractAddressConflictName and
// sync via the conflicting remote instead of syncing this (absent) name.
func BackupAddIfNotExists(ctx context.Context, db DBConn, name, url string) error {
	if err := BackupAdd(ctx, db, name, url); err != nil && !isBackupAlreadyExists(err) {
		return err
	}
	return nil
}

// BackupSync pushes the database to the named backup destination.
func BackupSync(ctx context.Context, db DBConn, name string) error {
	if _, err := db.ExecContext(ctx, "CALL DOLT_BACKUP('sync', ?)", name); err != nil {
		return fmt.Errorf("sync backup %s: %w", name, err)
	}
	return nil
}

// BackupRemove removes a configured Dolt backup destination.
func BackupRemove(ctx context.Context, db DBConn, name string) error {
	if _, err := db.ExecContext(ctx, "CALL DOLT_BACKUP('rm', ?)", name); err != nil {
		return fmt.Errorf("remove backup %s: %w", name, err)
	}
	return nil
}

// BackupRemoveIfExists removes a Dolt backup destination idempotently: removing
// a backup that is not registered — never added, or already removed by a
// concurrent process sharing the same sql-server — returns Dolt's "not found"
// error, which is treated as success.
func BackupRemoveIfExists(ctx context.Context, db DBConn, name string) error {
	if err := BackupRemove(ctx, db, name); err != nil && !isBackupNotFound(err) {
		return err
	}
	return nil
}

// BackupRestore restores a database from a backup at the given URL into
// the named database. When force is true, an existing database with the
// same name is overwritten. Mirrors the CLI: dolt backup restore [--force] <url> <db_name>
func BackupRestore(ctx context.Context, db DBConn, url, dbName string, force bool) error {
	if force {
		if _, err := db.ExecContext(ctx, "CALL DOLT_BACKUP('restore', '--force', ?, ?)", url, dbName); err != nil {
			return fmt.Errorf("restore from backup %s: %w", url, err)
		}
	} else {
		if _, err := db.ExecContext(ctx, "CALL DOLT_BACKUP('restore', ?, ?)", url, dbName); err != nil {
			return fmt.Errorf("restore from backup %s: %w", url, err)
		}
	}
	return nil
}

// DirToFileURL resolves dir to an absolute path and returns a file:// URL.
func DirToFileURL(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	return "file://" + abs, nil
}

// ExtractAddressConflictName parses the conflicting remote name from a Dolt
// "address conflict with a remote" error.
//
// Dolt returns errors of the form:
//
//	Error 1105: address conflict with a remote: 'name' -> url
//
// When BackupAdd fails because another remote (e.g. "default", registered by
// `bd backup init`) already points to the same URL, the caller can use the
// conflicting name to sync directly rather than treating it as a hard error.
// Returns "" if the error is not an address conflict.
func ExtractAddressConflictName(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const marker = "address conflict with a remote: '"
	idx := strings.Index(s, marker)
	if idx == -1 {
		return ""
	}
	s = s[idx+len(marker):]
	end := strings.Index(s, "'")
	if end == -1 {
		return ""
	}
	return s[:end]
}

// isBackupAlreadyExists reports whether err is Dolt's "backup '<name>' already
// exists" error, returned by DOLT_BACKUP('add', ...) when a backup with the
// same name is already registered. It does NOT match an address conflict
// (a different name pointing at the same URL); use ExtractAddressConflictName
// for that case.
func isBackupAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

// isBackupNotFound reports whether err is Dolt's "backup '<name>' not found"
// error, returned by DOLT_BACKUP('rm', ...) (and 'sync') when no backup with
// that name is registered.
func isBackupNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}
