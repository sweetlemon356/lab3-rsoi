package retryqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/sweetlemon356/lab3-rsoi/internal/gateway/application"
)

// RabbitMQ owns storage, delivery, acknowledgement and delayed redelivery.
type Queue struct {
	connection     *amqp.Connection
	consumer       *amqp.Channel
	name           string
	timeout        time.Duration
	publishTimeout time.Duration
	logger         *slog.Logger
}

type message struct {
	Kind     application.CleanupKind `json:"kind"`
	Username string                  `json:"username"`
	UID      string                  `json:"uid"`
}

func New(connection *amqp.Connection, name string, capacity int, interval, timeout, publishTimeout time.Duration, logger *slog.Logger) (*Queue, error) {
	channel, err := connection.Channel()
	if err != nil {
		return nil, err
	}
	q := &Queue{connection: connection, consumer: channel, name: name, timeout: timeout, publishTimeout: publishTimeout, logger: logger}
	work := queueArguments(name+".delay", capacity)
	if _, err = channel.QueueDeclare(name, true, false, false, false, work); err == nil {
		delay := queueArguments(name, capacity)
		delay["x-message-ttl"] = max(int64(1), interval.Milliseconds())
		_, err = channel.QueueDeclare(name+".delay", true, false, false, false, delay)
	}
	if err == nil {
		err = channel.Qos(1, 0, false)
	}
	if err != nil {
		channel.Close()
		return nil, fmt.Errorf("declare RabbitMQ cleanup queues: %w", err)
	}
	return q, nil
}

func queueArguments(target string, capacity int) amqp.Table {
	return amqp.Table{
		"x-queue-type":              "quorum",
		"x-max-length":              int64(capacity),
		"x-overflow":                "reject-publish",
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": target,
		"x-dead-letter-strategy":    "at-least-once",
		// Retries continue until the dependent service recovers.
		"x-delivery-limit": int32(-1),
	}
}

func (q *Queue) Enqueue(ctx context.Context, task application.CleanupTask) error {
	ctx, cancel := context.WithTimeout(ctx, q.publishTimeout)
	defer cancel()
	if err := q.Ping(ctx); err != nil {
		return err
	}
	payload, err := json.Marshal(message{Kind: task.Kind, Username: task.Username, UID: task.UID})
	if err != nil {
		return err
	}
	// One channel per publish keeps its return and confirmation unambiguous,
	// including concurrent compensation requests from different HTTP handlers.
	channel, err := q.connection.Channel()
	if err != nil {
		return err
	}
	defer channel.Close()
	if err := channel.Confirm(false); err != nil {
		return err
	}
	returned := channel.NotifyReturn(make(chan amqp.Return, 1))
	confirmation, err := channel.PublishWithDeferredConfirmWithContext(ctx, "", q.name+".delay", true, false, amqp.Publishing{
		ContentType: "application/json", DeliveryMode: amqp.Persistent, Body: payload,
	})
	if err != nil {
		return fmt.Errorf("publish cleanup: %w", err)
	}
	acknowledged, err := confirmation.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("confirm cleanup publication: %w", err)
	}
	select {
	case reply := <-returned:
		return fmt.Errorf("cleanup was not routed: %s", reply.ReplyText)
	default:
	}
	if !acknowledged {
		return errors.New("RabbitMQ rejected cleanup publication")
	}
	q.logger.Warn("cleanup queued", "kind", task.Kind, "uid", task.UID)
	return nil
}

func (q *Queue) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if q.connection.IsClosed() || q.consumer.IsClosed() {
		return errors.New("RabbitMQ unavailable")
	}
	return nil
}

func (q *Queue) Close() error { return q.consumer.Close() }

func (q *Queue) Run(ctx context.Context, execute func(context.Context, application.CleanupTask) error) error {
	consumer := "cleanup-" + uuid.NewString()
	deliveries, err := q.consumer.Consume(q.name, consumer, false, false, false, false, nil)
	if err != nil {
		return err
	}
	// Cancel synchronously before returning, so no background RPC races shutdown.
	defer q.consumer.Cancel(consumer, false)
	for {
		select {
		case <-ctx.Done():
			return nil
		case item, ok := <-deliveries:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("RabbitMQ cleanup consumer stopped")
			}
			var msg message
			err := json.Unmarshal(item.Body, &msg)
			if err == nil {
				attempt, cancel := context.WithTimeout(ctx, q.timeout)
				err = execute(attempt, application.CleanupTask{Kind: msg.Kind, Username: msg.Username, UID: msg.UID})
				cancel()
			}
			if ctx.Err() != nil {
				// Requeue an interrupted attempt; broker also requeues on connection loss.
				_ = item.Nack(false, true)
				return nil
			}
			if err != nil {
				// Rejection sends the message through the broker's delay queue.
				if rejectErr := item.Nack(false, false); rejectErr != nil {
					return rejectErr
				}
				q.logger.Warn("cleanup retry failed", "kind", msg.Kind, "uid", msg.UID, "error", err)
			} else {
				if err := item.Ack(false); err != nil {
					return err
				}
				q.logger.Info("cleanup completed", "kind", msg.Kind, "uid", msg.UID)
			}
		}
	}
}
