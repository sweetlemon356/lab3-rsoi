package retryqueue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	app "github.com/sweetlemon356/lab3-rsoi/internal/gateway/application"
	"github.com/sweetlemon356/lab3-rsoi/internal/platform/rabbitmqtest"
)

func testQueue(t *testing.T, capacity int, interval time.Duration) *Queue {
	t.Helper()
	connection, name := rabbitmqtest.Open(t)
	queue, err := New(connection, name, capacity, interval, 100*time.Millisecond, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })
	return queue
}

func awaitDelivery(t *testing.T, q *Queue) amqp.Delivery {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		item, ok, err := q.consumer.Get(q.name, false)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return item
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("message was not delivered")
	return amqp.Delivery{}
}

func TestBrokerDelaysFirstAttemptAndRejectedRetry(t *testing.T) {
	q := testQueue(t, 8, 300*time.Millisecond)
	ctx := context.Background()
	task := app.CleanupTask{Kind: app.CancelPayment, UID: "payment"}
	if err := q.Enqueue(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := q.consumer.Get(q.name, false); err != nil || ready {
		t.Fatalf("first attempt did not wait: ready=%v err=%v", ready, err)
	}
	item := awaitDelivery(t, q)
	var got message
	if err := json.Unmarshal(item.Body, &got); err != nil || got.UID != task.UID || got.Kind != task.Kind {
		t.Fatalf("message=%+v err=%v", got, err)
	}
	if err := item.Nack(false, false); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := q.consumer.Get(q.name, false); err != nil || ready {
		t.Fatalf("rejected attempt did not wait: ready=%v err=%v", ready, err)
	}
	if err := awaitDelivery(t, q).Ack(false); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRetriesUntilSuccessAndAcknowledges(t *testing.T) {
	q := testQueue(t, 8, 20*time.Millisecond)
	task := app.CleanupTask{Kind: app.RemoveLoyalty, Username: "user", UID: "booking"}
	if err := q.Enqueue(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	if err := q.Run(ctx, func(attempt context.Context, got app.CleanupTask) error {
		calls++
		if got != task {
			t.Errorf("task=%+v", got)
		}
		if _, ok := attempt.Deadline(); !ok {
			t.Error("attempt has no deadline")
		}
		if calls < 3 {
			return errors.New("offline")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("attempts=%d", calls)
	}
	for _, name := range []string{q.name, q.name + ".delay"} {
		state, err := q.consumer.QueueInspect(name)
		if err != nil || state.Messages != 0 {
			t.Fatalf("queue=%s messages=%d err=%v", name, state.Messages, err)
		}
	}
}

func TestBrokerRedeliversUnacknowledgedMessageAfterConnectionLoss(t *testing.T) {
	q := testQueue(t, 8, 20*time.Millisecond)
	if err := q.Enqueue(context.Background(), app.CleanupTask{Kind: app.CancelBooking, UID: "booking"}); err != nil {
		t.Fatal(err)
	}
	item := awaitDelivery(t, q)
	if _, ready, err := q.consumer.Get(q.name, false); err != nil || ready {
		t.Fatalf("unacknowledged message delivered twice concurrently: %v", err)
	}
	if err := q.connection.CloseDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	connection, err := amqp.Dial(os.Getenv("RABBITMQ_TEST_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	restarted, err := New(connection, q.name, 8, 20*time.Millisecond, time.Second, time.Second, q.logger)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	redelivered := awaitDelivery(t, restarted)
	if !redelivered.Redelivered || string(redelivered.Body) != string(item.Body) {
		t.Fatalf("unexpected redelivery: %+v", redelivered)
	}
	if err := redelivered.Ack(false); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerRejectsPublicationWhenDelayQueueIsFull(t *testing.T) {
	q := testQueue(t, 1, time.Hour)
	ctx := context.Background()
	if err := q.Enqueue(ctx, app.CleanupTask{Kind: app.CancelPayment, UID: "first"}); err != nil {
		t.Fatal(err)
	}
	// Quorum queues can briefly overshoot max-length before rejecting publishers.
	for range 10 {
		if err := q.Enqueue(ctx, app.CleanupTask{Kind: app.CancelPayment, UID: "overflow"}); err != nil {
			return
		}
	}
	t.Fatal("broker did not reject overflow")
}

func TestQueueHonorsCanceledPublicationAndClosedConnection(t *testing.T) {
	q := testQueue(t, 8, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Enqueue(ctx, app.CleanupTask{Kind: app.CancelPayment, UID: "payment"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publication: %v", err)
	}
	_ = q.connection.CloseDeadline(time.Now().Add(time.Second))
	if err := q.Enqueue(context.Background(), app.CleanupTask{Kind: app.CancelPayment, UID: "payment"}); err == nil {
		t.Fatal("closed broker connection accepted publication")
	}
}

func TestWorkerStopsInterruptedAttemptWithoutLosingMessage(t *testing.T) {
	q := testQueue(t, 8, 20*time.Millisecond)
	if err := q.Enqueue(context.Background(), app.CleanupTask{Kind: app.CancelPayment, UID: "payment"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	var calls atomic.Int32
	go func() {
		done <- q.Run(ctx, func(attempt context.Context, _ app.CleanupTask) error {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-attempt.Done()
			return attempt.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	// Cancelling the consumer and rejecting the attempt returns it to the broker.
	if err := awaitDelivery(t, q).Ack(false); err != nil {
		t.Fatal(err)
	}
}
