package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/testutil"
)

// testServerPort is the port of the shared test Dolt server.
var testServerPort int

// testSharedDB is the name of the shared database for branch-per-test isolation.
var testSharedDB string

// testSharedConn is a raw *sql.DB for branch operations in the shared database.
var testSharedConn *sql.DB

func TestMain(m *testing.M) {
	os.Exit(testMainInner(m))
}

func testMainInner(m *testing.M) int {
	os.Setenv("BEADS_TEST_MODE", "1")
	// Clear any ambient Dolt port env vars before container setup. This package
	// connects to the isolated test container via an explicit cfg.ServerPort (=
	// testServerPort) in initTrackerSharedSchema. applyConfigDefaults lets a set
	// BEADS_DOLT_SERVER_PORT (or legacy BEADS_DOLT_PORT) override that explicit
	// port, so an ambient value — e.g. the city/production Dolt server in a gc
	// session (BEADS_DOLT_SERVER_PORT=50215) — would silently redirect schema
	// init off the container and onto that server, where tracker_pkg_shared
	// doesn't exist. Unsetting here makes the container port authoritative
	// (matches be-n09's fix for internal/storage/dolt).
	os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	os.Unsetenv("BEADS_DOLT_PORT")
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: %v, skipping Dolt tests\n", err)
	} else {
		defer testutil.TerminateDoltContainer()
		testServerPort = testutil.DoltContainerPortInt()

		// Set up shared database for branch-per-test isolation
		testSharedDB = "tracker_pkg_shared"
		db, err := testutil.SetupSharedTestDB(testServerPort, testSharedDB)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: shared DB setup failed: %v\n", err)
			return 1
		}
		testSharedConn = db
		defer db.Close()

		// Create schema + config on the shared DB and commit to main
		if err := initTrackerSharedSchema(testServerPort); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: shared schema init failed: %v\n", err)
			return 1
		}
	}

	code := m.Run()

	os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	os.Unsetenv("BEADS_DOLT_PORT")
	os.Unsetenv("BEADS_TEST_MODE")
	return code
}

func initTrackerSharedSchema(port int) error {
	ctx := context.Background()
	cfg := &dolt.Config{
		Path:         "/tmp/tracker-shared-init",
		ServerHost:   "127.0.0.1",
		ServerPort:   port,
		Database:     testSharedDB,
		MaxOpenConns: 1,
	}
	store, err := dolt.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("New: %w", err)
	}
	defer store.Close()

	if err := store.SetConfig(ctx, "issue_prefix", "bd"); err != nil {
		return fmt.Errorf("SetConfig(issue_prefix): %w", err)
	}

	// Commit schema to main so branches get a clean snapshot
	db := store.DB()
	if _, err := db.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
		return fmt.Errorf("DOLT_ADD: %w", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('--allow-empty', '-m', 'test: init shared schema')"); err != nil {
		return fmt.Errorf("DOLT_COMMIT: %w", err)
	}
	if err := testutil.MaterializeLocalTableSchemasForBranchTests(ctx, db); err != nil {
		return fmt.Errorf("materialize local table schemas: %w", err)
	}

	return nil
}
