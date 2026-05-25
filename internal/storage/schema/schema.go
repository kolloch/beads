package schema

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type DBConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

//go:embed migrations/*.up.sql
var upMigrations embed.FS

//go:embed migrations/ignored/*.up.sql
var upIgnoredMigrations embed.FS

type migrationSource struct {
	files       embed.FS
	dir         string
	cursorTable string
}

var (
	mainSource = migrationSource{
		files:       upMigrations,
		dir:         "migrations",
		cursorTable: "schema_migrations",
	}
	ignoredSource = migrationSource{
		files:       upIgnoredMigrations,
		dir:         "migrations/ignored",
		cursorTable: "ignored_schema_migrations",
	}
)

var (
	latestOnce        sync.Once
	latestVer         int
	latestIgnoredOnce sync.Once
	latestIgnoredVer  int
)

func LatestVersion() int {
	latestOnce.Do(func() {
		latestVer = mainSource.latest()
	})
	return latestVer
}

func LatestIgnoredVersion() int {
	latestIgnoredOnce.Do(func() {
		latestIgnoredVer = ignoredSource.latest()
	})
	return latestIgnoredVer
}

func AllMigrationsSQL() string {
	var b strings.Builder
	b.WriteString(mainSource.bootstrapSQL())
	b.WriteString(";\n")
	for _, f := range mainSource.list() {
		data, err := mainSource.files.ReadFile(mainSource.dir + "/" + f.name)
		if err != nil {
			continue
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

func parseVersion(name string) (int, error) {
	parts := strings.SplitN(name, "_", 2)
	if len(parts) == 0 {
		return 0, fmt.Errorf("no version prefix")
	}
	return strconv.Atoi(parts[0])
}

// SchemaSkewError reports that the database has been migrated to a schema
// version newer than this bd binary understands. The binary's embedded queries
// assume the older schema, so proceeding would surface as cryptic
// column-not-found SQL errors instead of an actionable message (see be-930,
// where a stale binary hit a dropped generated column on every dependency
// query). Callers should treat this as fatal: the only fix is upgrading bd.
type SchemaSkewError struct {
	// DBVersion is the highest migration version recorded in the database.
	DBVersion int
	// BinaryVersion is the highest migration version this binary embeds.
	BinaryVersion int
}

func (e *SchemaSkewError) Error() string {
	return fmt.Sprintf(
		"database schema (v%d) is newer than this bd binary (knows migrations up to v%d): "+
			"your bd binary is out of date — rebuild or reinstall bd to match the database schema",
		e.DBVersion, e.BinaryVersion,
	)
}

// checkBinaryNotBehindDB fails fast when this source's version recorded in the
// database is newer than the highest migration the binary embeds for it. This
// is the binary-older-than-DB skew: MigrateUp's "already at latest" early
// return would otherwise let an out-of-date binary run queries against a schema
// it does not understand, producing cryptic SQL errors (be-930).
//
// A query error is treated as "no recorded version" (e.g. a fresh database
// whose cursor table does not exist yet) — that is not skew; migrate() creates
// the table.
func (m migrationSource) checkBinaryNotBehindDB(ctx context.Context, db DBConn) error {
	var dbVersion int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM "+m.cursorTable).Scan(&dbVersion); err != nil {
		return nil
	}
	if binaryVersion := m.latest(); dbVersion > binaryVersion {
		return &SchemaSkewError{DBVersion: dbVersion, BinaryVersion: binaryVersion}
	}
	return nil
}

func MigrateUp(ctx context.Context, db DBConn) (int, error) {
	// Detect binary-older-than-DB skew before the at-latest early return below:
	// when the DB has migrations this binary lacks, atLatest reports true and we
	// would silently proceed to run queries against an unknown schema. Only the
	// main schema_migrations cursor is guarded — that is the drift that produced
	// be-930's dropped-column errors. The inverse (binary newer than DB) is the
	// normal migrate path handled below.
	if err := mainSource.checkBinaryNotBehindDB(ctx, db); err != nil {
		return 0, err
	}

	if mainSource.atLatest(ctx, db) && ignoredSource.atLatest(ctx, db) {
		return 0, nil
	}

	applied, err := mainSource.migrate(ctx, db)
	if err != nil {
		return applied, err
	}

	backfilled, err := ensureBackfilledCustomStatusesCustomTypes(ctx, db)
	if err != nil {
		return applied, fmt.Errorf("backfill custom tables: %w", err)
	}

	if _, err := db.ExecContext(ctx, "REPLACE INTO dolt_ignore VALUES ('ignored_schema_migrations', true)"); err != nil {
		return applied, fmt.Errorf("registering ignored_schema_migrations in dolt_ignore: %w", err)
	}

	appliedIgnored, err := ignoredSource.migrate(ctx, db)
	if err != nil {
		return applied, fmt.Errorf("ignored migrations: %w", err)
	}

	if applied == 0 && !backfilled && appliedIgnored == 0 {
		return applied, nil
	}

	if _, err := db.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
		return applied, fmt.Errorf("staging migrations: %w", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-m', 'schema: apply migrations')"); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
			return applied, fmt.Errorf("committing migrations: %w", err)
		}
	}

	return applied, nil
}

type migrationFile struct {
	version int
	name    string
}

func (m migrationSource) bootstrapSQL() string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	version INT PRIMARY KEY,
	applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`, m.cursorTable)
}

func (m migrationSource) list() []migrationFile {
	entries, err := fs.ReadDir(m.files, m.dir)
	if err != nil {
		panic(fmt.Sprintf("schema: failed to read embedded %s: %v", m.dir, err))
	}
	var files []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		v, err := parseVersion(e.Name())
		if err != nil {
			panic(fmt.Sprintf("schema: invalid migration filename %q: %v", e.Name(), err))
		}
		files = append(files, migrationFile{version: v, name: e.Name()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files
}

func (m migrationSource) latest() int {
	files := m.list()
	if len(files) == 0 {
		return 0
	}
	return files[len(files)-1].version
}

func (m migrationSource) atLatest(ctx context.Context, db DBConn) bool {
	var current int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM "+m.cursorTable).Scan(&current); err != nil {
		return false
	}
	return current >= m.latest()
}

func (m migrationSource) migrate(ctx context.Context, db DBConn) (int, error) {
	if _, err := db.ExecContext(ctx, m.bootstrapSQL()); err != nil {
		return 0, fmt.Errorf("creating %s: %w", m.cursorTable, err)
	}

	var current int
	err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM "+m.cursorTable).Scan(&current)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("reading %s version: %w", m.cursorTable, err)
	}

	if current >= m.latest() {
		return 0, nil
	}

	count := 0
	for _, mf := range m.list() {
		if mf.version <= current {
			continue
		}
		data, err := m.files.ReadFile(m.dir + "/" + mf.name)
		if err != nil {
			return count, fmt.Errorf("reading migration %s: %w", mf.name, err)
		}
		if _, err := db.ExecContext(ctx, string(data)); err != nil {
			return count, fmt.Errorf("migration %s: %w", mf.name, err)
		}
		if _, err := db.ExecContext(ctx, "INSERT IGNORE INTO "+m.cursorTable+" (version) VALUES (?)", mf.version); err != nil {
			return count, fmt.Errorf("recording %s in %s: %w", mf.name, m.cursorTable, err)
		}
		count++
	}
	return count, nil
}
