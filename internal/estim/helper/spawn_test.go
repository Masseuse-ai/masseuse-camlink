package helper_test

import (
	"context"
	"os"
	"os/exec"

	"github.com/FemLed/masseuse-camlink/internal/estim/helper"
)

// startTestBinary runs this test binary as the helper program (TestMain
// serves when HELPER_TEST_GUEST is set), the way the Host's own start does.
func startTestBinary(_ context.Context, path, stateDir string) (helper.Process, error) {
	cmd := exec.Command(path, "--state-dir", stateDir)
	cmd.Env = append(os.Environ(), "HELPER_TEST_GUEST=1", "HELPER_TEST_STATE_DIR="+stateDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return helper.Process{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return helper.Process{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return helper.Process{}, err
	}
	if err := cmd.Start(); err != nil {
		return helper.Process{}, err
	}
	return helper.Process{
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
		Wait: cmd.Wait,
		Kill: func() error { _ = stdin.Close(); return cmd.Process.Kill() },
	}, nil
}
