package clients

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}
}
func request(method, host, path string) *http.Request {
	r, _ := http.NewRequest(method, "http://"+host+path, nil)
	return r
}
func call(t *testing.T, transport http.RoundTripper, r *http.Request) error {
	t.Helper()
	response, err := transport.RoundTrip(r)
	if response != nil {
		response.Body.Close()
	}
	return err
}

func TestCircuitThresholdIsolationAndWriteBypass(t *testing.T) {
	calls := map[string]int{}
	transport := NewCircuitTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) { calls[r.URL.Host]++; return reply(503), nil }), 3, 10*time.Second, time.Second)
	for range 3 {
		if err := call(t, transport, request("GET", "loyalty", "/read")); err != nil {
			t.Fatal(err)
		}
	}
	if err := call(t, transport, request("GET", "loyalty", "/other")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err=%v", err)
	}
	if calls["loyalty"] != 3 {
		t.Fatal(calls)
	}
	call(t, transport, request("GET", "payment", "/read"))
	call(t, transport, request("DELETE", "loyalty", "/write"))
	if calls["payment"] != 1 || calls["loyalty"] != 4 {
		t.Fatal(calls)
	}
}

func TestCircuitFailureCounter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []int
	}{
		{"success compensates one failure without resetting", []int{500, 500, 200, 500, 500}},
		{"not found compensates one failure", []int{500, 500, 404, 500, 500}},
		{"successes do not create credit below zero", []int{200, 404, 200, 500, 500, 500}},
		{"alternating results do not accumulate failures", []int{500, 200, 500, 404, 500, 200, 500, 500, 500}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := 200
			calls := 0
			transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return reply(status), nil
			}), 3, 10*time.Second, time.Second)
			for _, next := range tc.statuses {
				status = next
				if err := call(t, transport, request("GET", "backend", "/read")); err != nil {
					t.Fatalf("circuit opened before counter reached threshold: %v", err)
				}
			}
			if err := call(t, transport, request("GET", "backend", "/read")); !errors.Is(err, ErrCircuitOpen) {
				t.Fatalf("counter reaching threshold did not open circuit: %v", err)
			}
			if calls != len(tc.statuses) {
				t.Fatalf("blocked request reached backend: calls=%d", calls)
			}
		})
	}
}

func TestCircuitFailureObservationInterval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		elapsed  time.Duration
		wantOpen bool
	}{
		{"within interval", 9 * time.Second, true},
		{"at interval boundary", 10 * time.Second, true},
		{"after interval boundary", 10*time.Second + time.Nanosecond, false},
		{"after long idle period", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
				return reply(500), nil
			}), 2, 10*time.Second, time.Second)
			transport.now = func() time.Time { return now }
			if err := call(t, transport, request("GET", "backend", "/read")); err != nil {
				t.Fatal(err)
			}
			now = now.Add(tc.elapsed)
			if err := call(t, transport, request("GET", "backend", "/read")); err != nil {
				t.Fatal(err)
			}
			err := call(t, transport, request("GET", "backend", "/read"))
			if errors.Is(err, ErrCircuitOpen) != tc.wantOpen {
				t.Fatalf("after %s: err=%v, want open=%v", tc.elapsed, err, tc.wantOpen)
			}
			// If the old interval expired, the two recent failures now reach the threshold.
			if !tc.wantOpen {
				if err := call(t, transport, request("GET", "backend", "/read")); !errors.Is(err, ErrCircuitOpen) {
					t.Fatalf("new interval did not accumulate failures: %v", err)
				}
			}
		})
	}
}

func TestCircuitObservationIntervalStartsWithFirstResult(t *testing.T) {
	now := time.Now()
	status := 200
	transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return reply(status), nil
	}), 2, 10*time.Second, time.Second)
	transport.now = func() time.Time { return now }
	// Even a success starts the observation interval.
	call(t, transport, request("GET", "backend", "/read"))
	now = now.Add(9 * time.Second)
	status = 500
	call(t, transport, request("GET", "backend", "/read"))
	now = now.Add(2 * time.Second)
	for range 2 {
		if err := call(t, transport, request("GET", "backend", "/read")); err != nil {
			t.Fatalf("old interval contributed to new failures: %v", err)
		}
	}
	if err := call(t, transport, request("GET", "backend", "/read")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("new interval did not open circuit: %v", err)
	}
}

func TestCircuitSuccessfulProbeResetsFailureCounter(t *testing.T) {
	now := time.Now()
	status := 500
	transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { return reply(status), nil }), 2, 10*time.Second, time.Second)
	transport.now = func() time.Time { return now }
	for range 2 {
		if err := call(t, transport, request("GET", "backend", "/read")); err != nil {
			t.Fatal(err)
		}
	}
	if err := call(t, transport, request("GET", "backend", "/read")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	// Recover, then accumulate failures from zero.
	for _, next := range []int{200, 500, 404, 500, 500} {
		status = next
		if err := call(t, transport, request("GET", "backend", "/read")); err != nil {
			t.Fatalf("counter was not reset on recovery: %v", err)
		}
	}
	if err := call(t, transport, request("GET", "backend", "/read")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("counter did not open circuit after recovery: %v", err)
	}
}

func TestCircuitAllowsOneProbeAndIgnoresLateCompletions(t *testing.T) {
	now := time.Now()
	firstEntered, firstRelease := make(chan struct{}), make(chan struct{})
	probeEntered, probeRelease := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			<-firstRelease
			return reply(200), nil
		case 2:
			return reply(500), nil
		case 3:
			close(probeEntered)
			<-probeRelease
			return reply(200), nil
		default:
			return reply(200), nil
		}
	}), 1, 10*time.Second, time.Second)
	transport.now = func() time.Time { return now }
	done := make(chan struct{})
	doneProbe := make(chan struct{})
	go func() { defer close(done); call(t, transport, request("GET", "backend", "/read")) }()
	<-firstEntered
	call(t, transport, request("GET", "backend", "/read"))
	close(firstRelease)
	<-done
	if err := call(t, transport, request("GET", "backend", "/read")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("late success closed circuit: %v", err)
	}
	now = now.Add(time.Second)
	go func() { call(t, transport, request("GET", "backend", "/read")); close(doneProbe) }()
	<-probeEntered
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := call(t, transport, request("GET", "backend", "/read")); !errors.Is(err, ErrCircuitOpen) {
				t.Errorf("parallel probe err=%v", err)
			}
		}()
	}
	group.Wait()
	close(probeRelease)
	<-doneProbe
	if err := call(t, transport, request("GET", "backend", "/read")); err != nil {
		t.Fatal(err)
	}
}

func TestCircuitFailedProbeReopensAndQuoteIsProtected(t *testing.T) {
	now := time.Now()
	calls := 0
	transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("connection refused") }), 1, 10*time.Second, time.Second)
	transport.now = func() time.Time { return now }
	call(t, transport, request("POST", "backend", "/internal/v1/reservations/quote"))
	if err := call(t, transport, request("GET", "backend", "/read")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	call(t, transport, request("GET", "backend", "/read"))
	if err := call(t, transport, request("GET", "backend", "/read")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestCircuitCanceledCallerDoesNotCountAsBackendFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { cancel(); return nil, context.Canceled }), 1, 10*time.Second, time.Second)
	r := request("GET", "backend", "/read").WithContext(ctx)
	call(t, transport, r)
	transport.base = roundTripFunc(func(*http.Request) (*http.Response, error) { return reply(200), nil })
	if err := call(t, transport, request("GET", "backend", "/read")); err != nil {
		t.Fatal(err)
	}
}

func TestCircuitCountsHTTPClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client := &http.Client{Timeout: 20 * time.Millisecond, Transport: NewCircuitTransport(nil, 1, 10*time.Second, time.Second)}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("timeout expected")
	}
	response, err = client.Get(server.URL)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("timeout did not open circuit: %v", err)
	}
}

func TestCircuitCountsTimeoutWhileReadingResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &http.Client{Timeout: 50 * time.Millisecond, Transport: NewCircuitTransport(nil, 1, 10*time.Second, time.Second)}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err == nil {
		t.Fatal("body timeout expected")
	}
	response, err = client.Get(server.URL)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("body timeout did not open circuit: %v", err)
	}
}

func TestCircuitProbeRemainsExclusiveUntilBodyIsConsumed(t *testing.T) {
	now := time.Now()
	status := http.StatusServiceUnavailable
	transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return reply(status), nil
	}), 1, 10*time.Second, time.Second)
	transport.now = func() time.Time { return now }
	call(t, transport, request("GET", "backend", "/read"))
	now = now.Add(time.Second)
	status = http.StatusOK
	response, err := transport.RoundTrip(request("GET", "backend", "/read"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := call(t, transport, request("GET", "backend", "/other")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("probe released before body: %v", err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := call(t, transport, request("GET", "backend", "/other")); err != nil {
		t.Fatalf("successful probe did not close circuit: %v", err)
	}
}
