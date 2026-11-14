package application

import (
	"context"
	"strings"
	"time"
)

// Service coordinates Gateway operations through the three backend ports.
// Request-specific state stays in method locals, so one instance serves all requests.
type Service struct {
	reservations   ReservationPort
	payments       PaymentPort
	loyalty        LoyaltyPort
	newID          func() (string, error)
	cleanupTimeout time.Duration
	retryQueue     RetryQueue
}

func NewService(
	reservations ReservationPort,
	payments PaymentPort,
	loyalty LoyaltyPort,
	newID func() (string, error),
	cleanupTimeout time.Duration,
	options ...Option,
) *Service {
	service := &Service{
		reservations:   reservations,
		payments:       payments,
		loyalty:        loyalty,
		newID:          newID,
		cleanupTimeout: cleanupTimeout,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func validateUser(ctx context.Context, username string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(username) == "" {
		return &ValidationError{Fields: []FieldError{{Field: "X-User-Name", Error: "is required"}}}
	}
	return nil
}
