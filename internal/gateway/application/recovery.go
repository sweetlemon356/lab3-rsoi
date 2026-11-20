package application

import (
	"context"
	"errors"
)

type CleanupKind string

const (
	CancelPayment CleanupKind = "payment"
	CancelBooking CleanupKind = "reservation"
	RemoveLoyalty CleanupKind = "loyalty"
)

// CleanupTask is keyed by the original booking/payment identity; retries must
// never generate new identifiers or repeat a non-idempotent decrement.
type CleanupTask struct {
	Kind     CleanupKind
	Username string
	UID      string
}

type RetryQueue interface {
	Enqueue(context.Context, CleanupTask) error
}
type Option func(*Service)

func WithRetryQueue(queue RetryQueue) Option { return func(s *Service) { s.retryQueue = queue } }

func (s *Service) Cleanup(ctx context.Context, task CleanupTask) error {
	switch task.Kind {
	case CancelPayment:
		return s.payments.Cancel(ctx, task.UID)
	case CancelBooking:
		return s.reservations.Cancel(ctx, task.Username, task.UID)
	case RemoveLoyalty:
		return s.loyalty.RemoveReservation(ctx, task.Username, task.UID)
	default:
		return errors.New("unknown cleanup task")
	}
}

func (s *Service) deferCleanup(ctx context.Context, task CleanupTask) error {
	if s.retryQueue == nil {
		return errors.New("retry queue is not configured")
	}
	return s.retryQueue.Enqueue(ctx, task)
}

func unavailable(ctx context.Context, err error) bool {
	var dependency *DependencyError
	return ctx.Err() == nil && errors.As(err, &dependency)
}
