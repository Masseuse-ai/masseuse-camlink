package capture

import (
	"fmt"
	"os"
	"os/exec"
)

// execFFmpeg runs the real ffmpeg on args with this process's stdin, stdout
// and stderr and returns its exit code (Windows has no exec).
func execFFmpeg(path string, args []string) int {
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "run", path, err)
		return 1
	}
	return 0
}
