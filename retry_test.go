package ncemail

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestRetriesOnRateLimitThenSucceeds(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call < 3 {
			w.Header().Set("Retry-After", "2")
			jsonResponse(w, http.StatusTooManyRequests, `{"statusCode":429,"message":"Too many requests"}`)
			return
		}
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, sleeps := newTestClient(t, m.URL)

	resp, err := c.Emails.Send(context.Background(), validSend())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.ID != "abc" {
		t.Fatalf("id = %q", resp.ID)
	}
	if m.Count() != 3 {
		t.Fatalf("%d attempts, want 3", m.Count())
	}
	// Retry-After overrides the computed backoff outright.
	for i, d := range sleeps.Delays() {
		if d != 2*time.Second {
			t.Fatalf("delay %d = %v, want the server's 2s", i, d)
		}
	}
}

func TestRetryAfterAsHTTPDate(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 1 {
			// HTTP dates have one-second granularity, so the parsed delay is
			// this minus however much of the current second has elapsed.
			w.Header().Set("Retry-After", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
			jsonResponse(w, http.StatusTooManyRequests, `{"statusCode":429,"message":"slow down"}`)
			return
		}
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, sleeps := newTestClient(t, m.URL)

	if _, err := c.Emails.Send(context.Background(), validSend()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	delays := sleeps.Delays()
	if len(delays) != 1 {
		t.Fatalf("delays = %v", delays)
	}
	if delays[0] <= 0 || delays[0] > 4*time.Second {
		t.Fatalf("delay = %v, want roughly 3s from the HTTP date", delays[0])
	}
}

func TestRetryAfterIsClamped(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Retry-After", "3600")
		jsonResponse(w, http.StatusTooManyRequests, `{"statusCode":429,"message":"Too many requests"}`)
	})
	c, sleeps := newTestClient(t, m.URL)

	if _, err := c.Emails.Send(context.Background(), validSend()); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("want ErrRateLimit, got %v", err)
	}
	for _, d := range sleeps.Delays() {
		if d != retryAfterMax {
			t.Fatalf("delay = %v, want it clamped to %v", d, retryAfterMax)
		}
	}
}

func TestRetriesOnServerErrorAndTimeoutStatus(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
				jsonResponse(w, status, `{"statusCode":0,"message":"boom"}`)
			})
			c, _ := newTestClient(t, m.URL)

			if _, err := c.Emails.Send(context.Background(), validSend()); err == nil {
				t.Fatal("expected an error")
			}
			if m.Count() != 3 {
				t.Fatalf("%d attempts, want 3 (1 try + 2 retries)", m.Count())
			}
		})
	}
}

// A 403 on an unverified domain will never succeed. Retrying it only delays
// the error the caller needs to see.
func TestNoRetryOnClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
				jsonResponse(w, status, `{"statusCode":0,"message":"no"}`)
			})
			c, _ := newTestClient(t, m.URL)

			if _, err := c.Emails.Send(context.Background(), validSend()); err == nil {
				t.Fatal("expected an error")
			}
			if m.Count() != 1 {
				t.Fatalf("%d attempts for %d, want 1", m.Count(), status)
			}
		})
	}
}

// The whole point of the auto-generated key: three attempts of one Send are one
// send, so a timeout followed by a retry cannot double-mail a customer.
func TestIdempotencyKeyStableAcrossRetriesAndUniquePerCall(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusInternalServerError, `{"statusCode":500,"message":"boom"}`)
	})
	c, _ := newTestClient(t, m.URL)

	if _, err := c.Emails.Send(context.Background(), validSend()); err == nil {
		t.Fatal("expected an error")
	}
	first := m.Requests()
	if len(first) != 3 {
		t.Fatalf("%d attempts, want 3", len(first))
	}
	key := first[0].Header.Get("Idempotency-Key")
	if key == "" {
		t.Fatal("no idempotency key was sent")
	}
	for i, req := range first {
		if got := req.Header.Get("Idempotency-Key"); got != key {
			t.Fatalf("attempt %d used key %q, want %q", i, got, key)
		}
	}

	if _, err := c.Emails.Send(context.Background(), validSend()); err == nil {
		t.Fatal("expected an error")
	}
	second := m.Requests()[3:]
	if second[0].Header.Get("Idempotency-Key") == key {
		t.Fatal("a second Send reused the first call's key; that would suppress a genuinely new message")
	}
	for i, req := range second {
		if got := req.Header.Get("Idempotency-Key"); got != second[0].Header.Get("Idempotency-Key") {
			t.Fatalf("second call attempt %d drifted: %q", i, got)
		}
	}
}

// A *bytes.Reader is consumed by the first attempt, so the body has to be
// rebuilt each time. Without that the retry posts an empty body.
func TestRequestBodyIsRebuiltForEachAttempt(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call < 3 {
			jsonResponse(w, http.StatusInternalServerError, `{"statusCode":500,"message":"boom"}`)
			return
		}
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	if _, err := c.Emails.Send(context.Background(), validSend()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	reqs := m.Requests()
	for i, req := range reqs {
		if len(req.Body) == 0 {
			t.Fatalf("attempt %d posted an empty body", i)
		}
		if string(req.Body) != string(reqs[0].Body) {
			t.Fatalf("attempt %d posted a different body", i)
		}
	}
}

func TestRetriesDisabled(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusInternalServerError, `{"statusCode":500,"message":"boom"}`)
	})
	c, _ := newTestClient(t, m.URL, WithMaxRetries(0))

	if _, err := c.Emails.Send(context.Background(), validSend()); !errors.Is(err, ErrServer) {
		t.Fatalf("want ErrServer, got %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("%d attempts, want 1", m.Count())
	}
}

func TestBackoffIsFullJitterWithinTheWindow(t *testing.T) {
	c, err := New(testAPIKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for retry := 0; retry < 8; retry++ {
		window := c.backoffBase << uint(retry)
		if window > c.backoffCap {
			window = c.backoffCap
		}
		for i := 0; i < 200; i++ {
			d := c.retryDelay(retry, nil)
			if d < 0 || d > window {
				t.Fatalf("retry %d: delay %v outside [0, %v]", retry, d, window)
			}
		}
	}

	// A very high retry count must not overflow the shift into a negative or
	// zero window.
	if d := c.retryDelay(64, nil); d < 0 || d > c.backoffCap {
		t.Fatalf("delay %v outside the cap at a high retry count", d)
	}
}

// A cancelled context has to be honoured during the backoff, not only during
// the request: a bare time.Sleep here would park the caller's handler for the
// full delay after they had already given up.
func TestContextCancellationDuringBackoffReturnsPromptly(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusInternalServerError, `{"statusCode":500,"message":"boom"}`)
	})
	c, _ := newTestClient(t, m.URL)
	// The real waiter, and a backoff long enough that only cancellation can
	// end it.
	c.sleep = sleepWithContext
	c.backoffBase = 30 * time.Second
	c.backoffCap = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Emails.Send(ctx, validSend())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %v; cancellation was not honoured during the backoff", elapsed)
	}
	// The caller's own cancellation stays visible alongside the SDK's kind.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) is false for %v", err)
	}
	if !errors.Is(err, ErrConnection) {
		t.Fatalf("want ErrConnection, got %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("%d attempts; the retry should never have been made", m.Count())
	}
}

func TestContextDeadlineDuringBackoff(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusInternalServerError, `{"statusCode":500,"message":"boom"}`)
	})
	c, _ := newTestClient(t, m.URL)
	c.sleep = sleepWithContext
	c.backoffBase = 30 * time.Second
	c.backoffCap = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Emails.Send(ctx, validSend())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the context error should stay visible: %v", err)
	}
}

// A context that is already dead must not produce a request at all.
func TestAlreadyCancelledContext(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Emails.Send(ctx, validSend()); !errors.Is(err, context.Canceled) {
		t.Fatalf("want a cancellation error, got %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("%d requests were sent with a dead context", m.Count())
	}
}

// A refused connection is retryable and surfaces as ErrConnection, not as a
// bare *url.Error the caller has to type-assert.
func TestConnectionFailure(t *testing.T) {
	// Port 1 on loopback: nothing listens there, so this fails immediately and
	// deterministically without touching the network.
	c, _ := newTestClient(t, "http://127.0.0.1:1")

	_, err := c.Emails.Send(context.Background(), validSend())
	if !errors.Is(err, ErrConnection) {
		t.Fatalf("want ErrConnection, got %v", err)
	}
	if apiErr := mustAPIError(t, err); apiErr.StatusCode != 0 {
		t.Fatalf("StatusCode = %d, want 0", apiErr.StatusCode)
	}
}

func TestPerAttemptTimeout(t *testing.T) {
	release := make(chan struct{})
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) })

	c, _ := newTestClient(t, m.URL, WithTimeout(40*time.Millisecond), WithMaxRetries(0))

	_, err := c.Emails.Send(context.Background(), validSend())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}

// TGL-741: Retry-After is honoured on any retryable response, not just 429.
func TestRetryAfterHonouredOn503(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 1 {
			w.Header().Set("Retry-After", "7")
			jsonResponse(w, http.StatusServiceUnavailable, `{"message":"busy"}`)
			return
		}
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, log := newTestClient(t, m.URL)
	if _, err := c.Emails.Send(context.Background(), validSend()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if d := log.Delays(); len(d) != 1 || d[0] != 7*time.Second {
		t.Fatalf("delays = %v, want [7s]", d)
	}
}

// TGL-741: the exposed RateLimit.RetryAfter is clamped to 60s too.
func TestRateLimitRetryAfterFieldIsClamped(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Retry-After", "3600")
		jsonResponse(w, http.StatusTooManyRequests, `{"message":"slow down"}`)
	})
	c, _ := newTestClient(t, m.URL, WithMaxRetries(0))
	_, err := c.Emails.Send(context.Background(), validSend())
	apiErr := mustAPIError(t, err)
	if apiErr.RateLimit == nil || apiErr.RateLimit.RetryAfter != 60*time.Second {
		t.Fatalf("RateLimit = %+v", apiErr.RateLimit)
	}
}

// TGL-741: the timeout is a deadline on the whole attempt, including reading
// the body — a server trickling bytes cannot keep an attempt open past it.
func TestPerAttemptDeadlineCoversBodyRead(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 40; i++ {
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = w.Write([]byte(`{"id":"abc","status":"queued"}`))
	})
	c, _ := newTestClient(t, m.URL, WithTimeout(150*time.Millisecond), WithMaxRetries(0))
	start := time.Now()
	_, err := c.Emails.Send(context.Background(), validSend())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a trickling response: want ErrTimeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Fatalf("attempt ran %v, past its deadline", elapsed)
	}
}
