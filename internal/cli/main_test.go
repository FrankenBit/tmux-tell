package cli

import (
	"os"
	"testing"
)

// TestMain seeds the D-Bus session-bus env vars that startMailmanMissingEnv
// (#356) requires before any test runs. Tests that explicitly exercise the
// missing-env path (TestMCP_Register_SkipsMailmanWithMissingEnv,
// TestRegister_CLI_RefusesStartMailmanWithMissingEnv) override these via
// t.Setenv("DBUS_SESSION_BUS_ADDRESS", "") within their own scope.
//
// Without this, tests that drive the happy-path mailman-start flow fail in
// CI containers where pam_systemd has not run and the vars are absent.
func TestMain(m *testing.M) {
	os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	os.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	os.Exit(m.Run())
}
