package schema

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestCheckBinaryNotBehindDBDetectsSkew verifies that a database recorded at a
// version newer than the binary embeds is reported as a SchemaSkewError
// carrying both versions.
func TestCheckBinaryNotBehindDBDetectsSkew(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer db.Close()

	latest := LatestVersion()
	dbVersion := latest + 3
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", dbVersion)

	err = mainSource.checkBinaryNotBehindDB(context.Background(), db)

	var skew *SchemaSkewError
	if !errors.As(err, &skew) {
		t.Fatalf("checkBinaryNotBehindDB() error = %v, want *SchemaSkewError", err)
	}
	if skew.DBVersion != dbVersion {
		t.Errorf("SchemaSkewError.DBVersion = %d, want %d", skew.DBVersion, dbVersion)
	}
	if skew.BinaryVersion != latest {
		t.Errorf("SchemaSkewError.BinaryVersion = %d, want %d", skew.BinaryVersion, latest)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// TestCheckBinaryNotBehindDBAllowsEqualAndBehind verifies that a database at or
// behind the binary's embedded version is not treated as skew. Behind means the
// binary is newer and MigrateUp's normal path will apply the pending migrations.
func TestCheckBinaryNotBehindDBAllowsEqualAndBehind(t *testing.T) {
	latest := LatestVersion()
	for _, dbVersion := range []int{latest, latest - 1, 0} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("create sql mock: %v", err)
		}
		expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", dbVersion)

		if err := mainSource.checkBinaryNotBehindDB(context.Background(), db); err != nil {
			t.Errorf("checkBinaryNotBehindDB() with dbVersion=%d error = %v, want nil", dbVersion, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations for dbVersion=%d: %v", dbVersion, err)
		}
		db.Close()
	}
}

// TestCheckBinaryNotBehindDBToleratesMissingCursor verifies that a query error
// (e.g. a fresh database where schema_migrations does not exist yet) is treated
// as "no recorded version" rather than skew, so MigrateUp can create the table.
func TestCheckBinaryNotBehindDBToleratesMissingCursor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM schema_migrations")).
		WillReturnError(errors.New("Error 1146: Table 'beads.schema_migrations' doesn't exist"))

	if err := mainSource.checkBinaryNotBehindDB(context.Background(), db); err != nil {
		t.Fatalf("checkBinaryNotBehindDB() with missing cursor table error = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// TestMigrateUpFailsFastOnSchemaSkew verifies that MigrateUp surfaces the skew
// error before its at-latest early return and before touching any other query,
// so an out-of-date binary fails fast instead of running queries against an
// unknown schema (be-930).
func TestMigrateUpFailsFastOnSchemaSkew(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer db.Close()

	latest := LatestVersion()
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", latest+1)
	// No further queries are expected: the skew check must short-circuit.

	applied, err := MigrateUp(context.Background(), db)
	if applied != 0 {
		t.Errorf("MigrateUp() applied = %d, want 0", applied)
	}
	var skew *SchemaSkewError
	if !errors.As(err, &skew) {
		t.Fatalf("MigrateUp() error = %v, want *SchemaSkewError", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// TestSchemaSkewErrorMessageIsActionable verifies the operator-facing message
// names both versions and tells the user how to recover (upgrade bd), rather
// than leaking a raw SQL error.
func TestSchemaSkewErrorMessageIsActionable(t *testing.T) {
	msg := (&SchemaSkewError{DBVersion: 45, BinaryVersion: 42}).Error()

	for _, want := range []string{"v45", "v42", "out of date"} {
		if !strings.Contains(msg, want) {
			t.Errorf("SchemaSkewError message %q missing %q", msg, want)
		}
	}
	if !strings.Contains(msg, "rebuild") && !strings.Contains(msg, "reinstall") {
		t.Errorf("SchemaSkewError message %q does not tell the user to rebuild/reinstall", msg)
	}
}
