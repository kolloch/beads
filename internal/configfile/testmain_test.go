package configfile

import (
	"os"
	"testing"
)

// TestMain isolates this package's tests from ambient Dolt port env vars.
//
// GetDoltServerPort() reads BEADS_DOLT_SERVER_PORT (then legacy BEADS_DOLT_PORT)
// ahead of any config-derived or default value (see configfile.go). Many tests
// here assert the default (DefaultDoltServerPort) or a config-/credentials-keyed
// port for an explicit Config, e.g. TestDoltServerMode/GetDoltServerPort,
// TestDoltServerModeRoundtrip, TestEnvVarOverrides, and the credentials
// port-keyed lookups. When the process runs inside a gastown agent session the
// city/production Dolt server port is exported as BEADS_DOLT_SERVER_PORT
// (=50215), so those assertions silently read 50215 instead of the expected
// value and the tests fail. GitHub CI has no such ambient var, which is why this
// only bites local/gc runs.
//
// Clear both vars before the suite so the package's tests are hermetic. Tests
// that exercise the env-override path set the vars explicitly via t.Setenv,
// which restores them to "unset" afterwards — composing correctly with this
// clear. Same ambient-port-pollution class as be-n09 (internal/storage/dolt),
// be-aun (cmd/bd/doctor), and the BEADS_DIR clear in internal/config (be-jsr).
func TestMain(m *testing.M) {
	os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	os.Unsetenv("BEADS_DOLT_PORT")
	os.Exit(m.Run())
}
