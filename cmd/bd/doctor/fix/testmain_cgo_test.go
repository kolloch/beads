//go:build cgo

package fix

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
)

// TestMain starts an isolated Dolt server so fix tests don't hit the
// production server on port 3307.
func TestMain(m *testing.M) {
	os.Exit(testMainInner(m))
}

func testMainInner(m *testing.M) int {
	os.Setenv("BEADS_TEST_MODE", "1")
	// Clear any ambient Dolt port env vars before container setup so fix tests
	// connect to the isolated test container, not the production server.
	// applyConfigDefaults lets a set BEADS_DOLT_SERVER_PORT (or legacy
	// BEADS_DOLT_PORT) override an explicit port, so an ambient value — e.g. the
	// city/production Dolt server in a gc session — would silently redirect tests
	// onto that server. Unsetting here makes the container authoritative (matches
	// be-n09's fix for internal/storage/dolt TestMain).
	os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	os.Unsetenv("BEADS_DOLT_PORT")
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: %v, skipping Dolt tests\n", err)
	} else {
		defer testutil.TerminateDoltContainer()
	}

	code := m.Run()

	os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	os.Unsetenv("BEADS_DOLT_PORT")
	os.Unsetenv("BEADS_TEST_MODE")
	return code
}
