package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	app "github.com/sweetlemon356/lab3-rsoi/internal/gateway/application"
)

type queueFake struct {
	tasks []app.CleanupTask
	err   error
}

func (q *queueFake) Enqueue(_ context.Context, task app.CleanupTask) error {
	if q.err != nil {
		return q.err
	}
	q.tasks = append(q.tasks, task)
	return nil
}
func offline(service string) error {
	return &app.DependencyError{Service: service, Err: errors.New("offline")}
}

func TestNoncriticalFailuresKeepBookingDataAndMarkMissingParts(t *testing.T) {
	r := reservationFake{
		list: func(context.Context, string) ([]app.ReservationData, error) {
			return []app.ReservationData{reservation()}, nil
		},
		get: func(context.Context, string, string) (app.ReservationData, error) { return reservation(), nil },
	}
	p := paymentFake{get: func(context.Context, string) (app.PaymentInfo, error) { return app.PaymentInfo{}, offline("payment") }}
	l := loyaltyFake{get: func(context.Context, string) (app.LoyaltyInfo, error) { return app.LoyaltyInfo{}, offline("loyalty") }}
	service := newService(r, p, l)
	me, err := service.GetUserInfo(context.Background(), username)
	if err != nil || !me.LoyaltyUnavailable || len(me.Reservations) != 1 || !me.Reservations[0].PaymentUnavailable || me.Reservations[0].Hotel.HotelUID != hotelID {
		t.Fatalf("me=%+v err=%v", me, err)
	}
	got, err := service.GetReservation(context.Background(), username, reservationID)
	if err != nil || !got.PaymentUnavailable || got.ReservationUID != reservationID {
		t.Fatalf("booking=%+v err=%v", got, err)
	}
	if _, err := service.GetLoyalty(context.Background(), username); err == nil {
		t.Fatal("critical loyalty failure was hidden")
	}
}

func TestCancelDefersUnavailableLoyaltyButStillCancelsBooking(t *testing.T) {
	f := newBookingFixture(t)
	f.paid, f.saved, f.counted = true, true, true
	f.l.remove = func(context.Context, string, string) error { return offline("loyalty") }
	queue := &queueFake{}
	s := app.NewService(f.r, f.p, f.l, nil, time.Second, app.WithRetryQueue(queue))
	if err := s.CancelReservation(context.Background(), username, reservationID); err != nil {
		t.Fatal(err)
	}
	if f.paid || f.saved || !f.counted || len(queue.tasks) != 1 {
		t.Fatalf("state paid=%v saved=%v counted=%v queued=%v", f.paid, f.saved, f.counted, queue.tasks)
	}
	task := queue.tasks[0]
	if task.Kind != app.RemoveLoyalty || task.UID != reservationID || task.Username != username {
		t.Fatalf("task=%+v", task)
	}
	// The worker uses the same scenario and original identity after recovery.
	f.l.remove = func(context.Context, string, string) error { f.counted = false; return nil }
	recovered := newService(f.r, f.p, f.l)
	if err := recovered.Cleanup(context.Background(), task); err != nil || f.counted {
		t.Fatalf("cleanup err=%v counted=%v", err, f.counted)
	}
}

func TestQueueFullDoesNotReportSuccessfulCancellation(t *testing.T) {
	f := newBookingFixture(t)
	f.paid, f.saved, f.counted = true, true, true
	f.l.remove = func(context.Context, string, string) error { return offline("loyalty") }
	full := errors.New("queue full")
	s := app.NewService(f.r, f.p, f.l, nil, time.Second, app.WithRetryQueue(&queueFake{err: full}))
	if err := s.CancelReservation(context.Background(), username, reservationID); !errors.Is(err, full) {
		t.Fatal(err)
	}
}

func TestBookingRollbackQueuesFailedCompensations(t *testing.T) {
	f := newBookingFixture(t)
	f.fail = "loyalty"
	f.cleanupFailure = offline("loyalty")
	queue := &queueFake{}
	ids := 0
	s := app.NewService(f.r, f.p, f.l, func() (string, error) {
		ids++
		if ids == 1 {
			return reservationID, nil
		}
		return paymentID, nil
	}, time.Second, app.WithRetryQueue(queue))
	if _, err := s.CreateReservation(context.Background(), createCommand()); !errors.Is(err, f.failure) {
		t.Fatal(err)
	}
	if len(queue.tasks) != 3 {
		t.Fatalf("queued=%v", queue.tasks)
	}
	for _, task := range queue.tasks {
		if task.UID != reservationID && task.UID != paymentID {
			t.Fatalf("new identity=%+v", task)
		}
	}
}

func TestPaymentCancellationFailureDoesNotQueueLoyaltyOrCancelBooking(t *testing.T) {
	f := newBookingFixture(t)
	f.saved, f.counted = true, true
	f.p.cancel = func(context.Context, string) error { return offline("payment") }
	queue := &queueFake{}
	s := app.NewService(f.r, f.p, f.l, nil, time.Second, app.WithRetryQueue(queue))
	if err := s.CancelReservation(context.Background(), username, reservationID); err == nil {
		t.Fatal("payment failure hidden")
	}
	if !f.saved || !f.counted || len(queue.tasks) != 0 {
		t.Fatal("later steps ran after payment failure")
	}
}
