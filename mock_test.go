package ncemail

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The contract fixes the literal used in tests, so a grep for a real key
// prefix across the repos never turns up a test fixture and wastes an hour.
const (
	testAPIKey    = "nmail_live_test0000000000000000"
	testKeySecret = "test0000000000000000"
)

type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// mockAPI is a real HTTP server on loopback rather than a stubbed
// http.RoundTripper, so the tests exercise the transport the SDK actually
// ships with — redirect policy, header canonicalisation, connection reuse and
// all. It needs no network access beyond the loopback interface.
type mockAPI struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
	calls    int
}

func newMockAPI(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, call int)) *mockAPI {
	t.Helper()
	m := &mockAPI{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		m.mu.Lock()
		m.calls++
		call := m.calls
		m.requests = append(m.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		})
		m.mu.Unlock()

		handler(w, r, call)
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mockAPI) Requests() []recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]recordedRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

func (m *mockAPI) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// jsonResponse is the shape every handler below writes.
func jsonResponse(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// sleepLog replaces the client's backoff wait so a test can assert the delay
// the retry loop computed without spending it. Nothing else in the retry path
// is stubbed.
type sleepLog struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *sleepLog) record(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return contextError(ctx)
	}
	s.mu.Lock()
	s.delays = append(s.delays, d)
	s.mu.Unlock()
	return nil
}

func (s *sleepLog) Delays() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.delays))
	copy(out, s.delays)
	return out
}

func newTestClient(t *testing.T, baseURL string, opts ...Option) (*Client, *sleepLog) {
	t.Helper()
	all := make([]Option, 0, len(opts)+1)
	all = append(all, WithBaseURL(baseURL))
	all = append(all, opts...)

	c, err := New(testAPIKey, all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log := &sleepLog{}
	c.sleep = log.record
	return c, log
}

// validSend is the minimum request that passes local validation.
func validSend() *SendEmailRequest {
	return &SendEmailRequest{
		From:    "Acme <hello@acme.com>",
		To:      []string{"customer@example.com"},
		Subject: "Your receipt",
		HTML:    "<p>Thanks for your order.</p>",
	}
}

// mustAPIError asserts the error is the SDK's type and returns it.
func mustAPIError(t *testing.T, err error) *APIError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	return apiErr
}
