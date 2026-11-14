package retryqueue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/sweetlemon356/lab3-rsoi/internal/gateway/application"
)

// Queue is process-local. Pending tasks are deduplicated, bounded, and never
// removed on failure. Only one worker executes a task at a time.
type Queue struct {
	mu                sync.Mutex
	pending           map[application.CleanupTask]time.Time
	capacity          int
	interval, timeout time.Duration
	logger            *slog.Logger
}

func New(capacity int, interval, timeout time.Duration, logger *slog.Logger) *Queue {
	return &Queue{pending: make(map[application.CleanupTask]time.Time), capacity: capacity, interval: interval, timeout: timeout, logger: logger}
}

func (q *Queue) Enqueue(task application.CleanupTask) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.pending[task]; exists {
		return nil
	}
	if len(q.pending) >= q.capacity {
		return errors.New("retry queue is full")
	}
	q.pending[task] = time.Now().Add(q.interval)
	q.logger.Warn("cleanup queued", "kind", task.Kind, "uid", task.UID, "pending", len(q.pending))
	return nil
}

func (q *Queue) Run(ctx context.Context, execute func(context.Context, application.CleanupTask) error) {
	ticker := time.NewTicker(min(q.interval, 100*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			q.mu.Lock()
			tasks := make([]application.CleanupTask, 0, len(q.pending))
			for task, due := range q.pending {
				if !now.Before(due) {
					tasks = append(tasks, task)
				}
			}
			q.mu.Unlock()
			for _, task := range tasks {
				if ctx.Err() != nil {
					return
				}
				attempt, cancel := context.WithTimeout(ctx, q.timeout)
				err := execute(attempt, task)
				cancel()
				q.mu.Lock()
				if err == nil {
					delete(q.pending, task)
				} else {
					q.pending[task] = time.Now().Add(q.interval)
				}
				q.mu.Unlock()
				if err != nil {
					q.logger.Warn("cleanup retry failed", "kind", task.Kind, "uid", task.UID, "error", err)
				} else {
					q.logger.Info("cleanup completed", "kind", task.Kind, "uid", task.UID)
				}
			}
		}
	}
}
