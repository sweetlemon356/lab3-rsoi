package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	CircuitThreshold, RetryCapacity                                int
	CircuitWindow, CircuitTimeout, RetryInterval                   time.Duration
	Address                                                        string
	RabbitMQURL                                                    string
	BrokerTimeout                                                  time.Duration
	ReservationURL, PaymentURL, LoyaltyURL                         string
	RequestTimeout, ClientTimeout, CleanupTimeout, ShutdownTimeout time.Duration
}

func Load() (Config, error) {
	cfg := Config{Address: environment("GATEWAY_ADDR", ":8080")}
	_, port, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return Config{}, fmt.Errorf("GATEWAY_ADDR must be host:port: %w", err)
	}
	if !validPort(port) {
		return Config{}, fmt.Errorf("GATEWAY_ADDR port must be between 1 and 65535")
	}
	cfg.RabbitMQURL = environment("RABBITMQ_URL", "amqp://booking:booking@localhost:5672/")
	brokerURL, err := url.Parse(cfg.RabbitMQURL)
	if err != nil || (brokerURL.Scheme != "amqp" && brokerURL.Scheme != "amqps") || brokerURL.Hostname() == "" || brokerURL.RawQuery != "" || brokerURL.Fragment != "" || brokerURL.ForceQuery {
		return Config{}, fmt.Errorf("RABBITMQ_URL must be an AMQP URL without query or fragment")
	}
	if brokerURL.Port() != "" && !validPort(brokerURL.Port()) {
		return Config{}, fmt.Errorf("RABBITMQ_URL has an invalid port")
	}
	for _, item := range []struct {
		key, fallback string
		target        *string
	}{
		{"RESERVATION_URL", "http://localhost:8070", &cfg.ReservationURL},
		{"PAYMENT_URL", "http://localhost:8060", &cfg.PaymentURL},
		{"LOYALTY_URL", "http://localhost:8050", &cfg.LoyaltyURL},
	} {
		value := environment(item.key, item.fallback)
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return Config{}, fmt.Errorf("%s must be an HTTP(S) URL without credentials, query or fragment", item.key)
		}
		if parsed.Port() != "" && !validPort(parsed.Port()) {
			return Config{}, fmt.Errorf("%s has an invalid port", item.key)
		}
		*item.target = strings.TrimRight(value, "/")
	}
	for _, item := range []struct {
		key      string
		fallback time.Duration
		target   *time.Duration
	}{
		{"REQUEST_TIMEOUT", 5 * time.Second, &cfg.RequestTimeout},
		{"CLIENT_TIMEOUT", 2 * time.Second, &cfg.ClientTimeout},
		{"CLEANUP_TIMEOUT", 2 * time.Second, &cfg.CleanupTimeout},
		{"SHUTDOWN_TIMEOUT", 10 * time.Second, &cfg.ShutdownTimeout},
		{"CIRCUIT_TIMEOUT", 10 * time.Second, &cfg.CircuitTimeout},
		{"CIRCUIT_WINDOW", 10 * time.Second, &cfg.CircuitWindow},
		{"RETRY_INTERVAL", 10 * time.Second, &cfg.RetryInterval},
		{"BROKER_TIMEOUT", 2 * time.Second, &cfg.BrokerTimeout},
	} {
		value, err := time.ParseDuration(environment(item.key, item.fallback.String()))
		if err != nil || value <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive duration", item.key)
		}
		*item.target = value
	}
	for _, item := range []struct {
		key      string
		fallback int
		target   *int
	}{
		{"CIRCUIT_THRESHOLD", 3, &cfg.CircuitThreshold},
		{"RETRY_CAPACITY", 1024, &cfg.RetryCapacity},
	} {
		value, err := strconv.Atoi(environment(item.key, strconv.Itoa(item.fallback)))
		if err != nil || value <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive integer", item.key)
		}
		*item.target = value
	}
	if cfg.ShutdownTimeout < cfg.CleanupTimeout {
		return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT must be at least CLEANUP_TIMEOUT")
	}
	return cfg, nil
}

func environment(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func validPort(value string) bool {
	port, err := strconv.Atoi(value)
	return err == nil && port >= 1 && port <= 65535
}
