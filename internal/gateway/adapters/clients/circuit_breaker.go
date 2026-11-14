package clients

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var ErrCircuitOpen = errors.New("circuit breaker is open")

type circuitState struct {
	firstResultAt time.Time
	lastResultAt  time.Time
	failures      int
	retryAt       time.Time
	probing       bool
	generation    uint64
}

type CircuitTransport struct {
	base      http.RoundTripper
	threshold int
	window    time.Duration
	cooldown  time.Duration
	now       func() time.Time
	mu        sync.Mutex
	states    map[string]*circuitState
}

func NewCircuitTransport(base http.RoundTripper, threshold int, window, cooldown time.Duration) *CircuitTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	if threshold <= 0 {
		threshold = 3
	}
	if window <= 0 {
		window = 10 * time.Second
	}
	if cooldown <= 0 {
		cooldown = 10 * time.Second
	}
	return &CircuitTransport{base: base, threshold: threshold, window: window, cooldown: cooldown, now: time.Now, states: make(map[string]*circuitState)}
}

func (t *CircuitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet && !(r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reservations/quote")) {
		return t.base.RoundTrip(r)
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	key := r.URL.Scheme + "://" + r.URL.Host
	t.mu.Lock()
	state := t.states[key]
	if state == nil {
		state = &circuitState{}
		t.states[key] = state
	}
	if !state.retryAt.IsZero() {
		if t.now().Before(state.retryAt) || state.probing {
			t.mu.Unlock()
			return nil, ErrCircuitOpen
		}
		state.probing = true
	}
	generation := state.generation
	probe := state.probing
	t.mu.Unlock()
	response, err := t.base.RoundTrip(r)
	if err != nil || response.StatusCode >= 500 {
		t.complete(r, state, generation, probe, true)
	} else {
		response.Body = &circuitBody{ReadCloser: response.Body, complete: func(failed bool) {
			t.complete(r, state, generation, probe, failed)
		}}
	}
	return response, err
}

func (t *CircuitTransport) complete(r *http.Request, state *circuitState, generation uint64, probe, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if state.generation != generation {
		return
	}
	if errors.Is(r.Context().Err(), context.Canceled) && !probe {
		return
	}
	now := t.now()
	if probe {
		state.probing = false
		state.generation++
		if failed {
			state.retryAt = now.Add(t.cooldown)
		} else {
			state.retryAt = time.Time{}
			state.failures = 0
			state.firstResultAt = time.Time{}
			state.lastResultAt = time.Time{}
		}
	} else {
		if state.firstResultAt.IsZero() || now.Sub(state.firstResultAt) > t.window {
			state.firstResultAt = now
			state.failures = 0
		}
		state.lastResultAt = now
		if failed {
			if state.failures < t.threshold {
				state.failures++
			}
		} else if state.failures > 0 {
			state.failures--
		}
		if state.failures >= t.threshold && state.lastResultAt.Sub(state.firstResultAt) <= t.window {
			state.retryAt = now.Add(t.cooldown)
			state.generation++
		}
	}
}

func (t *CircuitTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type circuitBody struct {
	io.ReadCloser
	once     sync.Once
	complete func(bool)
}

func (b *circuitBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(func() { b.complete(err != io.EOF) })
	}
	return n, err
}

func (b *circuitBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.complete(err != nil) })
	return err
}
