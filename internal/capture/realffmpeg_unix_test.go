//go:build !windows

package capture

import (
	"fmt"
	"os"
	"syscall"
)

// execFFmpeg replaces this process with the real ffmpeg on args, so the
// connector's stdin (the "q" that ends it) and stderr reach ffmpeg itself.
func execFFmpeg(path string, args []string) int {
	if err := syscall.Exec(path, append([]string{path}, args...), os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "exec", path, err)
	}
	return 1
}
