package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// launchedFromBundle reports whether this process was opened from the
// application bundle rather than started from a shell: the executable is a
// bundle's (or LaunchServices set __CFBundleIdentifier, as it does for
// every app it starts) and standard input is not a terminal. Run from a
// terminal, the very same binary inside the bundle is a console program.
func launchedFromBundle() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if !bundleExecutable(exe) && os.Getenv("__CFBundleIdentifier") == "" {
		return false
	}
	return !isTerminal(os.Stdin)
}

// isTerminal is isatty(3): a file that answers the terminal attributes
// ioctl. LaunchServices gives an app /dev/null, which is a character
// device but not a terminal, so the file mode alone would not do.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}

// handToTerminal is what the bundle does when opened: if a connector is
// already running on this state directory, bring Terminal forward and
// leave it be; otherwise write the .command file and open it, so Terminal
// runs this program in a window of its own. Either way this process ends.
func handToTerminal(stateDir string) error {
	release, err := lockInstance(stateDir)
	if err != nil {
		if errors.Is(err, errAlreadyRunning) {
			return run("/usr/bin/open", "-a", "Terminal")
		}
		return err
	}
	release()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	path := filepath.Join(stateDir, commandFile)
	if err := os.WriteFile(path, []byte(commandScript(exe, stateDir)), 0o755); err != nil {
		return err
	}
	// WriteFile's mode applies only when the file is created; an older copy
	// keeps its own, so Terminal is made sure of an executable file.
	if err := os.Chmod(path, 0o755); err != nil {
		return err
	}
	if err := run("/usr/bin/open", path); err != nil {
		// Whatever claimed .command files could not open it: Terminal
		// itself can.
		if err := run("/usr/bin/open", "-b", "com.apple.Terminal", path); err != nil {
			return fmt.Errorf("could not open a Terminal window for %s: %w", path, err)
		}
	}
	return nil
}

// reportHandoffFailure tells the person, who has no terminal to read, that
// the program could not hand itself to one, and what to run instead.
func reportHandoffFailure(err error) {
	exe, _ := os.Executable()
	message := fmt.Sprintf("%v. To run it by hand, open Terminal and enter: %s", err, shellQuote(exe))
	_ = run("/usr/bin/osascript", "-e", fmt.Sprintf("display alert %q message %q as critical", appName+" could not open a Terminal window", message))
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, string(out))
	}
	return nil
}
