//go:build cgo

package utils

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
)

// testServerPort is the port of the isolated test Dolt server (0 = not running).
// Set by TestMain before tests run so that newTestStore connects to the test
// server instead of the production Dolt server on port 3307.
var testServerPort int

func TestMain(m *testing.M) {
	os.Exit(testMainInner(m))
}

func testMainInner(m *testing.M) int {
	os.Setenv("BEADS_TEST_MODE", "1")
	// Clear any ambient Dolt port env vars before container setup. Tests in this
	// package connect via an explicit cfg.ServerPort (= testServerPort, the
	// isolated test container). applyConfigDefaults lets a set
	// BEADS_DOLT_SERVER_PORT (or legacy BEADS_DOLT_PORT) override that explicit
	// port, so an ambient value — e.g. the city/production Dolt server in a gc
	// session — would silently redirect every test off the container and onto
	// that server. Unsetting here makes testServerPort authoritative for the
	// whole package (matches be-n09's fix for internal/storage/dolt TestMain).
	os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	os.Unsetenv("BEADS_DOLT_PORT")
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: %v, skipping Dolt tests\n", err)
	} else {
		defer testutil.TerminateDoltContainer()
		testServerPort = testutil.DoltContainerPortInt()
	}

	code := m.Run()

	testServerPort = 0
	os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	os.Unsetenv("BEADS_DOLT_PORT")
	os.Unsetenv("BEADS_TEST_MODE")
	return code
}
