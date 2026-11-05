package beads

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync/atomic"
	"testing"
	"time"
)

type idleProbeConn struct{ closed *atomic.Int32 }

func (c idleProbeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c idleProbeConn) Close() error                        { c.closed.Add(1); return nil }
func (c idleProbeConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c idleProbeConn) Ping(context.Context) error          { return nil }

type idleProbeConnector struct{ closed *atomic.Int32 }

func (c idleProbeConnector) Connect(context.Context) (driver.Conn, error) {
	return idleProbeConn(c), nil
}
func (c idleProbeConnector) Driver() driver.Driver { return nil }

type idleProbeStorage struct{ db *sql.DB }

func (s idleProbeStorage) DB() *sql.DB { return s.db }

// pingAndIdle opens one pooled connection, returns it to the pool, waits, and
// reports how many physical connections the pool closed meanwhile. It returns
// as soon as one is closed; if none is, it returns 0 after the full wait.
// database/sql runs its idle cleaner at most once a second, hence the long wait.
// Polls on a timer instead of time.Sleep: the resource census ratchets direct
// time.Sleep calls in tests and fails the PR if the count grows.
func pingAndIdle(t *testing.T, db *sql.DB, closed *atomic.Int32, wait time.Duration) int32 {
	t.Helper()
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			return closed.Load()
		case <-poll.C:
			if n := closed.Load(); n > 0 {
				return n
			}
		}
	}
}

func TestBoundNativeConnIdleRetiresIdleConnections(t *testing.T) {
	var closed atomic.Int32
	db := sql.OpenDB(idleProbeConnector{closed: &closed})
	defer func() { _ = db.Close() }()
	boundNativeConnIdle(idleProbeStorage{db: db}, 50*time.Millisecond)
	if got := pingAndIdle(t, db, &closed, 2200*time.Millisecond); got == 0 {
		t.Fatal("idle connection was not retired after the bound")
	}
}

// Negative control: without the bound the pool keeps the idle connection.
func TestBoundNativeConnIdleNegativeControl(t *testing.T) {
	var closed atomic.Int32
	db := sql.OpenDB(idleProbeConnector{closed: &closed})
	defer func() { _ = db.Close() }()
	if got := pingAndIdle(t, db, &closed, 2200*time.Millisecond); got != 0 {
		t.Fatalf("unbounded pool closed %d idle connections", got)
	}
}

func TestBoundNativeConnIdleIgnoresStorageWithoutDB(_ *testing.T) {
	boundNativeConnIdle(struct{}{}, time.Second)
	boundNativeConnIdle(idleProbeStorage{}, time.Second)
}

// The pool idle bound must stay under the managed Dolt listener read_timeout,
// or the server reaps idle sockets before the pool retires them (gcf-b3kkh).
func TestNativeDoltConnMaxIdleBelowReadTimeout(t *testing.T) {
	// internal/config imports this package, so mirror config.DefaultDoltReadTimeoutMillis.
	readTimeout := 15000 * time.Millisecond
	if nativeDoltConnMaxIdleTime >= readTimeout/2 {
		t.Fatalf("idle %v must be < half of read_timeout %v", nativeDoltConnMaxIdleTime, readTimeout)
	}
}
