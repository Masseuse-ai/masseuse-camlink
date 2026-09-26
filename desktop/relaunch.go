package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The connector keeps the program current (cmd/masseuse-camlink/update.go,
// README "Updates"): it downloads and verifies a newer release, swaps it
// in at the install root the shell named (-install-root: this bundle, this
// .exe, this directory), and ends with relaunchExitCode for the shell that
// started it to run the new version. The shell is that program: it quits,
// and once the window is gone starts itself again from the same place, now
// the new version. The start goes through a small helper that waits a
// second first, so this process has ended and let go of its single-instance
// lock before the next one takes it.

const (
	// relaunchEnv is set in the connector's environment so it knows a
	// shell will start it again (internal/update, RelaunchEnv).
	relaunchEnv = "MASSEUSE_CAMLINK_RELAUNCH"
	// relaunchExitCode is the connector's exit code asking for it
	// (internal/update, RelaunchExitCode).
	relaunchExitCode = 75
)

// relaunchProgram starts this program again after a second: the bundle
// through LaunchServices on a Mac, the executable at its own path
// otherwise, with the same arguments.
func relaunchProgram(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		if root := bundleRoot(exe); root != "" {
			cmd = exec.Command("/bin/sh", "-c", `sleep 1; exec /usr/bin/open "$0"`, root)
		} else {
			cmd = exec.Command("/bin/sh", append([]string{"-c", `sleep 1; exec "$0" "$@"`, exe}, args...)...)
		}
	case "windows":
		script := fmt.Sprintf("Start-Sleep -Seconds 1; Start-Process -FilePath %s", psQuote(exe))
		if len(args) > 0 {
			quoted := make([]string, len(args))
			for i, a := range args {
				quoted[i] = psQuote(a)
			}
			script += " -ArgumentList " + strings.Join(quoted, ",")
		}
		cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
		hideWindow(cmd)
	default:
		cmd = exec.Command("/bin/sh", append([]string{"-c", `sleep 1; exec "$0" "$@"`, exe}, args...)...)
	}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Not waited on: the helper outlives this process.
	return cmd.Process.Release()
}

// bundleRoot is the .app the executable lies in, or "".
func bundleRoot(exe string) string {
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" && strings.HasSuffix(filepath.Dir(filepath.Dir(dir)), ".app") {
		return filepath.Dir(filepath.Dir(dir))
	}
	return ""
}

// psQuote quotes s as a PowerShell single-quoted string.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
