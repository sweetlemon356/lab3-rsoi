package application

import (
	"context"
	"errors"
	"fmt"
)

func (s *Service) CancelReservation(ctx context.Context, username, reservationUID string) error {
	if err := validateUser(ctx, username); err != nil {
		return err
	}
	row, err := s.reservations.GetByUser(ctx, username, reservationUID)
	if err != nil {
		return fmt.Errorf("get reservation to cancel: %w", err)
	}
	if row.Username != username || row.ReservationUID != reservationUID {
		return ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Always reconcile all effects, including for CANCELED reservations. Previous
	// cancellation/compensation may have completed only part of the workflow.
	if err := s.payments.Cancel(ctx, row.PaymentUID); err != nil {
		return fmt.Errorf("cancel payment: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.loyalty.RemoveReservation(ctx, username, reservationUID); err != nil {
		if !unavailable(ctx, err) {
			return fmt.Errorf("cancel loyalty accounting: %w", err)
		}
		if queueErr := s.deferCleanup(ctx, CleanupTask{Kind: RemoveLoyalty, Username: username, UID: reservationUID}); queueErr != nil {
			return fmt.Errorf("defer loyalty cancellation: %w", errors.Join(err, queueErr))
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.reservations.Cancel(ctx, username, reservationUID); err != nil {
		return fmt.Errorf("cancel reservation: %w", err)
	}
	return ctx.Err()
}
