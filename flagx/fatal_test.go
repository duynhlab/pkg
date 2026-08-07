package flagx

// The Must* variants log.Fatal on invalid input; exercising them requires the
// subprocess re-exec pattern (same as temporalx's versioning tests): the
// helper branch runs in a child process and is expected to exit non-zero.

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// expectFatal re-runs the named test in a subprocess with env set and asserts
// the child exits non-zero (log.Fatal).
func expectFatal(t *testing.T, testName string, env ...string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run="+testName+"$")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("subprocess exited 0, want fatal; output: %s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.Success() {
		t.Fatalf("subprocess error = %v, want non-zero exit; output: %s", err, out)
	}
}

func TestMustEnum_FatalOnInvalidValue(t *testing.T) {
	if os.Getenv("FLAGX_FATAL_HELPER") == "1" {
		MustEnum("FLAGX_TEST_MODE", "off", "off", "on")
		return // unreachable: the value below is invalid
	}
	expectFatal(t, "TestMustEnum_FatalOnInvalidValue",
		"FLAGX_FATAL_HELPER=1", "FLAGX_TEST_MODE=bogus")
}

func TestMustPercent_FatalOnOutOfRange(t *testing.T) {
	if os.Getenv("FLAGX_FATAL_HELPER") == "1" {
		MustPercent("FLAGX_TEST_PERCENT", 50)
		return // unreachable: the value below is out of range
	}
	expectFatal(t, "TestMustPercent_FatalOnOutOfRange",
		"FLAGX_FATAL_HELPER=1", "FLAGX_TEST_PERCENT=150")
}
