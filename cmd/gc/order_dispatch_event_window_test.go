package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// execEnvCapture records the env of every exec-order run.
type execEnvCapture struct{ runs [][]string }

func (c *execEnvCapture) run(_ context.Context, _, _ string, env []string) ([]byte, error) {
	c.runs = append(c.runs, append([]string(nil), env...))
	return []byte("ok"), nil
}

func envLookup(env []string, key string) (string, bool) {
	val, found := "", false
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			val, found = v, true
		}
	}
	return val, found
}

func TestOrderDispatchEventExecExportsEventWindow(t *testing.T) {
	store := beads.NewMemStore()
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	firstHead, err := eventLog.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq(): %v", err)
	}

	var capture execEnvCapture
	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:    "window-exec",
		Trigger: "event",
		On:      events.BeadClosed,
		Exec:    "scripts/window.sh",
	}}, store, eventLog, capture.run, events.Discard)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	secondHead, err := eventLog.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq(): %v", err)
	}
	ad.dispatch(context.Background(), t.TempDir(), time.Now().Add(10*time.Second))
	ad.drain(context.Background())

	if len(capture.runs) != 2 {
		t.Fatalf("exec runs = %d, want 2", len(capture.runs))
	}
	for i, want := range []struct{ after, head uint64 }{
		{0, firstHead},
		{firstHead, secondHead},
	} {
		after, okAfter := envLookup(capture.runs[i], orders.EventAfterSeqEnv)
		head, okHead := envLookup(capture.runs[i], orders.EventHeadSeqEnv)
		if !okAfter || !okHead {
			t.Fatalf("run %d env missing event window: after=%v head=%v", i, okAfter, okHead)
		}
		if after != fmt.Sprint(want.after) || head != fmt.Sprint(want.head) {
			t.Fatalf("run %d event window = (%s, %s], want (%d, %d]", i, after, head, want.after, want.head)
		}
	}
}

func TestOrderDispatchCooldownExecHasNoEventWindow(t *testing.T) {
	store := beads.NewMemStore()
	var capture execEnvCapture
	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:     "tick-exec",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "scripts/tick.sh",
	}}, store, events.NewFake(), capture.run, events.Discard)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	if len(capture.runs) != 1 {
		t.Fatalf("exec runs = %d, want 1", len(capture.runs))
	}
	for _, key := range []string{orders.EventAfterSeqEnv, orders.EventHeadSeqEnv} {
		if v, ok := envLookup(capture.runs[0], key); ok {
			t.Fatalf("cooldown exec env carries %s=%q; want it only on event-triggered runs", key, v)
		}
	}
}

func TestOrderEventWindowEnvKeysAreReserved(t *testing.T) {
	for _, key := range []string{orders.EventAfterSeqEnv, orders.EventHeadSeqEnv} {
		if !orders.IsReservedExecEnvKey(key) {
			t.Fatalf("%s is not reserved; an [order.env] entry could spoof the controller's event window", key)
		}
	}
}
