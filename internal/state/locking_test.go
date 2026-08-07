package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Locking tests: the store's SQLite operational policy (WAL + busy_timeout)
// must keep readers unblocked and bound writer contention. Both stores below
// use their own single connection, mirroring `runstead run` and
// `runstead inspect` as separate processes.

func TestWALReadersAreNotBlockedByWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstead.db")
	writer, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()
	reader, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer reader.Close()

	mustTask(t, writer, "task-1")
	tx, err := writer.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin write tx: %v", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = 'completed' WHERE task_id = 'task-1'`); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The reader must observe the pre-transaction state without blocking.
	var status string
	if err := reader.db.QueryRow(`SELECT status FROM tasks WHERE task_id = 'task-1'`).Scan(&status); err != nil {
		t.Fatalf("reader query: %v", err)
	}
	if status != "running" {
		t.Fatalf("reader saw %q, want running (uncommitted write must be invisible)", status)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

func TestBusyTimeoutBindsContendedWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstead.db")
	first, err := Open(Options{Path: path, Clock: newFixedClock(), BusyTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	defer first.Close()
	second, err := Open(Options{Path: path, Clock: newFixedClock(), BusyTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer second.Close()

	mustTask(t, first, "task-1")
	tx, err := first.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO actions (action_id, task_id, action_sequence, tool, status, created_at)
		VALUES ('action-hold', 'task-1', 1, 't', 'planned', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The second connection's write must wait for the busy timeout, then fail
	// cleanly with a locked error instead of hanging forever.
	start := time.Now()
	_, err = second.db.Exec(`INSERT INTO actions (action_id, task_id, action_sequence, tool, status, created_at)
		VALUES ('action-hold-2', 'task-1', 2, 't', 'planned', '2026-01-01T00:00:00Z')`)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("contended write must fail while the first transaction is open")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "locked") {
		t.Fatalf("contended write error = %v, want a locked error", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("contended write failed too early (%v), busy timeout not honored", elapsed)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}
