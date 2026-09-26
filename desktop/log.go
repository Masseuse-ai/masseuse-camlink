package main

import (
	"os"
	"sync"
)

// logFileName is the connector's log under the state directory: its
// standard error, and the shell's own lines. Help, "Show the log" reveals
// it. It is capped: past logMaxBytes the file is moved aside as .1 and a
// new one started, so a long-running window never fills a disk.
const (
	logFileName = "desktop.log"
	logMaxBytes = 4 << 20
)

// logFile is a size-capped append-only file.
type logFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openLogFile(path string) (*logFile, error) {
	lf := &logFile{path: path}
	if err := lf.open(); err != nil {
		return nil, err
	}
	return lf, nil
}

func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *logFile) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(b), nil
	}
	if l.size+int64(len(b)) > logMaxBytes {
		l.f.Close()
		_ = os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			l.f = nil
			return len(b), nil
		}
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	return n, err
}

func (l *logFile) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}
