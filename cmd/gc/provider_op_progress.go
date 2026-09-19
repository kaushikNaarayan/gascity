package main

import (
	"context"
	"fmt"
	"time"
)

// providerOpProgressConfig bounds a progress-aware provider op (currently
// only "init", gcf-6ryi). A fixed deadline cannot tell a slow-but-advancing
// fresh init (bd's baseline migration is thousands of statements, each
// gated by Dolt commit latency on the data disk) from a hung one. Instead the
// op may run while its process group keeps making I/O progress, up to ceiling,
// and is killed once it makes none for stall.
type providerOpProgressConfig struct {
	stall   time.Duration // no progress for this long => fail fast
	ceiling time.Duration // hard upper bound on total elapsed time
	poll    time.Duration // sampling interval
}

var providerInitProgress = providerOpProgressConfig{
	stall:   60 * time.Second,
	ceiling: 15 * time.Minute,
	poll:    2 * time.Second,
}

// providerOpProgressSample is a monotonic activity reading for a process
// group: I/O bytes (rchar+wchar) and I/O syscalls (syscr+syscw). Any change in
// either counts as progress.
type providerOpProgressSample struct {
	Bytes    uint64
	Syscalls uint64
}

// providerOpSampleProgress returns the activity reading for the process group
// led by pgid; ok is false when unavailable on this platform. A var so tests
// can drive progress deterministically.
var providerOpSampleProgress = sampleProcessGroupProgress

// providerOpStallError is the cause attached to the op context when the
// no-progress window elapses. It unwraps to context.DeadlineExceeded so callers
// that test for a deadline keep working.
type providerOpStallError struct {
	op        string
	elapsed   time.Duration
	sinceLast time.Duration
	stall     time.Duration
	last      providerOpProgressSample
}

func (e *providerOpStallError) Error() string {
	return fmt.Sprintf("%s made no progress for %s (window %s; elapsed %s; last observed I/O: %d bytes, %d syscalls)",
		e.op, e.sinceLast.Round(time.Second), e.stall, e.elapsed.Round(time.Second), e.last.Bytes, e.last.Syscalls)
}

func (e *providerOpStallError) Unwrap() error { return context.DeadlineExceeded }

// superviseProviderOpProgress samples the process group every cfg.poll and
// cancels the op with a providerOpStallError when the counter has not
// advanced for cfg.stall. It returns when done is closed or it has canceled.
func superviseProviderOpProgress(op string, pgid int, cfg providerOpProgressConfig, sample func(int) (providerOpProgressSample, bool), done <-chan struct{}, cancel context.CancelCauseFunc) {
	start := time.Now()
	lastProgress := start
	var last providerOpProgressSample
	if v, ok := sample(pgid); ok {
		last = v
	}
	t := time.NewTicker(cfg.poll)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			if v, ok := sample(pgid); ok && v != last {
				last = v
				lastProgress = now
			}
			if since := now.Sub(lastProgress); since >= cfg.stall {
				cancel(&providerOpStallError{op: op, elapsed: now.Sub(start), sinceLast: since, stall: cfg.stall, last: last})
				return
			}
		}
	}
}
