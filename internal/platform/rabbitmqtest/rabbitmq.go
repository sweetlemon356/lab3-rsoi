// Package rabbitmqtest opens isolated durable queues on a disposable broker.
package rabbitmqtest

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

func Open(t *testing.T) (*amqp.Connection, string) {
	t.Helper()
	address := os.Getenv("RABBITMQ_TEST_URL")
	if address == "" {
		t.Skip("set RABBITMQ_TEST_URL for tests against a disposable RabbitMQ broker")
	}
	connection, err := amqp.DialConfig(address, amqp.Config{
		Dial: amqp.DefaultDial(2 * time.Second), Heartbeat: time.Second,
		Recovery: &amqp.Recovery{},
	})
	if err != nil {
		t.Fatal(err)
	}
	name := "cleanup-test." + uuid.NewString()
	t.Cleanup(func() {
		_ = connection.CloseDeadline(time.Now().Add(time.Second))
		cleanup, err := amqp.DialConfig(address, amqp.Config{Dial: amqp.DefaultDial(2 * time.Second)})
		if err != nil {
			t.Errorf("connect to clean test queues: %v", err)
			return
		}
		defer cleanup.CloseDeadline(time.Now().Add(time.Second))
		for _, queue := range []string{name, name + ".delay"} {
			channel, err := cleanup.Channel()
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := channel.QueueDelete(queue, false, false, false); err != nil {
				t.Errorf("delete test queue: %v", err)
			}
			channel.Close()
		}
	})
	return connection, name
}
