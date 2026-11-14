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
	transport := NewCircuitTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) { calls[r.URL.Host]++; return reply(503), nil }), 3, time.Second)
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

func TestCircuitSuccessAndNotFoundResetConsecutiveFailures(t *testing.T) {
	status := 500
	transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { return reply(status), nil }), 2, time.Second)
	for _, next := range []int{500, 404, 500, 200, 500, 500} {
		status = next
		if err := call(t, transport, request("GET", "reservation", "/read")); err != nil {
			t.Fatal(err)
		}
	}
	if err := call(t, transport, request("GET", "reservation", "/read")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal(err)
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
	}), 1, time.Second)
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
	transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("connection refused") }), 1, time.Second)
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
	transport := NewCircuitTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { cancel(); return nil, context.Canceled }), 1, time.Second)
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
	client := &http.Client{Timeout: 20 * time.Millisecond, Transport: NewCircuitTransport(nil, 1, time.Second)}
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
	client := &http.Client{Timeout: 50 * time.Millisecond, Transport: NewCircuitTransport(nil, 1, time.Second)}
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
	}), 1, time.Second)
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
