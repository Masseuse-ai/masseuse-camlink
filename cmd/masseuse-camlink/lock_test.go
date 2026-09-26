package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeExecutable(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

func TestLockInstance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state") // created by the lock
	release, err := lockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFile)); err != nil {
		t.Fatalf("lock file: %v", err)
	}
	if _, err := lockInstance(dir); !errors.Is(err, errAlreadyRunning) {
		t.Fatalf("second lock: err = %v, want errAlreadyRunning", err)
	}
	release()
	again, err := lockInstance(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}
