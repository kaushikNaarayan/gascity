package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// checkStartedSignal is an event-driven "the condition check has started"
// signal for tests: the check shell command writes to a FIFO, and wait blocks
// reading it, so no test polls the filesystem on a sleep loop.
type checkStartedSignal struct {
	path string
}

func newCheckStartedSignal(t *testing.T, dir, name string) checkStartedSignal {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	return checkStartedSignal{path: path}
}

// shell is the command fragment the condition check runs once it has started.
// It blocks until a wait call is reading the FIFO.
func (s checkStartedSignal) shell() string { return "echo started > '" + s.path + "'" }

// wait blocks until a check has signalled, or reports false after timeout.
func (s checkStartedSignal) wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.Open(s.path) // blocks until a writer opens the FIFO
		if err != nil {
			return
		}
		buf := make([]byte, 8)
		_, _ = f.Read(buf)
		_ = f.Close()
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		// Release the reader goroutine blocked in open.
		if w, err := os.OpenFile(s.path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		<-done
		return false
	}
}
