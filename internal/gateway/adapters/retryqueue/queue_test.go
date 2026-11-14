package retryqueue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	app "github.com/sweetlemon356/lab3-rsoi/internal/gateway/application"
)

func TestQueueDeduplicatesAndKeepsFailingTasksUntilRecovery(t *testing.T) {
	q := New(1, 5*time.Millisecond, 100*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	task := app.CleanupTask{Kind: app.RemoveLoyalty, Username: "user", UID: "booking"}
	if err := q.Enqueue(task); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := q.Enqueue(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Enqueue(app.CleanupTask{Kind: app.RemoveLoyalty, UID: "other"}); err == nil {
		t.Fatal("capacity not enforced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	success := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		q.Run(ctx, func(attempt context.Context, got app.CleanupTask) error {
			if got != task {
				t.Errorf("task=%+v", got)
			}
			if _, ok := attempt.Deadline(); !ok {
				t.Error("attempt has no deadline")
			}
			if calls.Add(1) < 3 {
				return errors.New("offline")
			}
			close(success)
			return nil
		})
	}()
	select {
	case <-success:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not recover")
	}
	cancel()
	<-done
	q.mu.Lock()
	pending := len(q.pending)
	q.mu.Unlock()
	if pending != 0 || calls.Load() != 3 {
		t.Fatalf("pending=%d calls=%d", pending, calls.Load())
	}
}

func TestQueueStopsInFlightAttemptOnShutdown(t *testing.T) {
	q := New(1, time.Millisecond, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.Enqueue(app.CleanupTask{Kind: app.CancelPayment, UID: "payment"})
	ctx, cancel := context.WithCancel(context.Background())
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		q.Run(ctx, func(attempt context.Context, _ app.CleanupTask) error {
			close(started)
			<-attempt.Done()
			return attempt.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked")
	}
	q.mu.Lock()
	pending := len(q.pending)
	q.mu.Unlock()
	if pending != 1 {
		t.Fatal("failed task was lost")
	}
}
