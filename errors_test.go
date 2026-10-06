package ncemail

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStatusToErrorKind(t *testing.T) {
	cases := []struct {
		status  int
		body    string
		want    error
		message string
	}{
		{400, `{"statusCode":400,"message":"\"to\" is required","error":"Bad Request"}`, ErrValidation, `"to" is required`},
		{401, `{"statusCode":401,"message":"invalid API key","error":"Unauthorized"}`, ErrAuthentication, "invalid API key"},
		{403, `{"statusCode":403,"message":"not allowed to send from \"x@y.com\". Verify the domain first.","error":"Forbidden"}`, ErrPermission, `not allowed to send from "x@y.com". Verify the domain first.`},
		{404, `{"statusCode":404,"message":"Cannot POST /v1/emails","error":"Not Found"}`, ErrNotFound, "Cannot POST /v1/emails"},
		{408, `{"statusCode":408,"message":"Request Timeout"}`, ErrTimeout, "Request Timeout"},
		{409, `{"statusCode":409,"message":"duplicate idempotency key","error":"Conflict"}`, ErrConflict, "duplicate idempotency key"},
		{422, `{"statusCode":422,"message":"unprocessable"}`, ErrValidation, "unprocessable"},
		{429, `{"statusCode":429,"message":"Too many requests"}`, ErrRateLimit, "Too many requests"},
		{500, `{"statusCode":500,"message":"Internal server error"}`, ErrServer, "Internal server error"},
		{502, `<html>bad gateway</html>`, ErrServer, ""},
		{503, `{"statusCode":503,"message":"unavailable"}`, ErrServer, "unavailable"},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
				w.Header().Set("X-Request-Id", "req_123")
				jsonResponse(w, tc.status, tc.body)
			})
			// Retries off: this test is about the mapping, not the loop.
			c, _ := newTestClient(t, m.URL, WithMaxRetries(0))

			_, err := c.Emails.Send(context.Background(), validSend())
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: want %v, got %v", tc.status, tc.want, err)
			}

			apiErr := mustAPIError(t, err)
			if apiErr.StatusCode != tc.status {
				t.Fatalf("StatusCode = %d", apiErr.StatusCode)
			}
			if tc.message != "" && apiErr.Message != tc.message {
				t.Fatalf("Message = %q, want %q", apiErr.Message, tc.message)
			}
			if apiErr.RequestID != "req_123" {
				t.Fatalf("RequestID = %q", apiErr.RequestID)
			}
			if string(apiErr.Body) != tc.body {
				t.Fatalf("Body = %q", apiErr.Body)
			}
			// Nothing the SDK renders may carry the credential.
			if strings.Contains(apiErr.Error(), testKeySecret) {
				t.Fatalf("error text leaked the key: %s", apiErr.Error())
			}
		})
	}
}

// The control plane answers an unknown message id with 400 and the literal
// text "message not found" instead of a 404. A caller checking for ErrNotFound
// after a Get is writing the obvious thing, so it has to work.
func TestBadRequestMessageNotFoundMapsToNotFound(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusBadRequest, `{"statusCode":400,"message":"message not found","error":"Bad Request"}`)
	})
	c, _ := newTestClient(t, m.URL)

	_, err := c.Emails.Get(context.Background(), "5b1e")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if errors.Is(err, ErrValidation) {
		t.Fatal("it must not also be a validation error")
	}
	if got := mustAPIError(t, err).StatusCode; got != 400 {
		t.Fatalf("StatusCode = %d; the real status must survive the remap", got)
	}
}

// A different 400 stays a validation error, so the string match above cannot
// swallow genuine input errors.
func TestOtherBadRequestStaysValidation(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusBadRequest, `{"statusCode":400,"message":"attachments[0] content is not valid base64","error":"Bad Request"}`)
	})
	c, _ := newTestClient(t, m.URL)

	_, err := c.Emails.Send(context.Background(), validSend())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("an ordinary 400 must not be remapped to not-found")
	}
}

// NestJS sends an array of strings when a validation pipe rejects several
// fields at once.
func TestArrayMessageJoined(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusBadRequest, `{"statusCode":400,"message":["from must be an email","to should not be empty"],"error":"Bad Request"}`)
	})
	c, _ := newTestClient(t, m.URL)

	_, err := c.Emails.Send(context.Background(), validSend())
	apiErr := mustAPIError(t, err)
	if apiErr.Message != "from must be an email; to should not be empty" {
		t.Fatalf("Message = %q", apiErr.Message)
	}
}

// A proxy or load balancer in front of the API can answer with anything at
// all. That must produce a usable error, never a parse failure.
func TestNonJSONErrorBodyFallsBackToStatusLine(t *testing.T) {
	for name, body := range map[string]string{
		"html":  "<html><body>502 Bad Gateway</body></html>",
		"empty": "",
		"array": `["not","our","shape"]`,
	} {
		t.Run(name, func(t *testing.T) {
			m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(body))
			})
			c, _ := newTestClient(t, m.URL, WithMaxRetries(0))

			_, err := c.Emails.Send(context.Background(), validSend())
			if !errors.Is(err, ErrServer) {
				t.Fatalf("want ErrServer, got %v", err)
			}
			apiErr := mustAPIError(t, err)
			if apiErr.Message == "" {
				t.Fatal("Message must never be empty")
			}
			if !strings.Contains(apiErr.Error(), "502") {
				t.Fatalf("error should quote the status: %s", apiErr.Error())
			}
		})
	}
}

// Following a redirect would re-send Authorization to whatever host the
// Location header names, which is how bearer tokens leak.
func TestRedirectIsRefused(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Location", "https://attacker.example.com/v1/emails")
		w.WriteHeader(http.StatusFound)
	})
	c, _ := newTestClient(t, m.URL)

	_, err := c.Emails.Send(context.Background(), validSend())
	if !errors.Is(err, ErrServer) {
		t.Fatalf("want ErrServer, got %v", err)
	}
	if !strings.Contains(err.Error(), "unexpected redirect") {
		t.Fatalf("error should say why: %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("%d requests; a redirect must not be followed or retried", m.Count())
	}
}

func TestRateLimitDetailIsReachable(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.Header().Set("Retry-After", "7")
		jsonResponse(w, http.StatusTooManyRequests, `{"statusCode":429,"message":"Too many requests"}`)
	})
	c, _ := newTestClient(t, m.URL, WithMaxRetries(0))

	_, err := c.Emails.Send(context.Background(), validSend())

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*APIError) failed for %v", err)
	}
	if apiErr.RateLimit == nil || apiErr.RateLimit.RetryAfter != 7*time.Second {
		t.Fatalf("RateLimit = %+v", apiErr.RateLimit)
	}

	// The detail is also reachable through errors.As on its own type.
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Fatalf("errors.As(*RateLimitError) failed for %v", err)
	}
	if apiErr.Kind() != ErrRateLimit {
		t.Fatalf("Kind() = %v", apiErr.Kind())
	}
}

func TestAPIErrorText(t *testing.T) {
	cases := []struct {
		err  *APIError
		want string
	}{
		{&APIError{Message: "bad input", kind: ErrValidation}, "ncemail: bad input"},
		{&APIError{Message: "nope", StatusCode: 403, ErrorLabel: "Forbidden", kind: ErrPermission}, "ncemail: 403 Forbidden: nope"},
		{&APIError{Message: "nope", StatusCode: 500, kind: ErrServer, RequestID: "req_9"}, "ncemail: 500: nope (request id req_9)"},
		{&APIError{StatusCode: 500, kind: ErrServer}, "ncemail: 500: request failed"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Fatalf("Error() = %q, want %q", got, tc.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		in    string
		want  time.Duration
		ok    bool
		label string
	}{
		{"", 0, false, "absent"},
		{"7", 7 * time.Second, true, "seconds"},
		{"  7  ", 7 * time.Second, true, "padded seconds"},
		{"-5", 0, true, "negative seconds"},
		{"3600", retryAfterMax, true, "clamped"},
		{"garbage", 0, false, "unparseable"},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second, true, "http date"},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0, true, "past http date"},
		{now.Add(2 * time.Hour).Format(http.TimeFormat), retryAfterMax, true, "clamped http date"},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.in, now)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestDecodeFailureIsAServerError(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":`)
	})
	c, _ := newTestClient(t, m.URL)

	_, err := c.Emails.Send(context.Background(), validSend())
	if !errors.Is(err, ErrServer) {
		t.Fatalf("want ErrServer, got %v", err)
	}
	// Not retried: the send may well have been accepted, and repeating it
	// would only produce the same undecodable answer.
	if m.Count() != 1 {
		t.Fatalf("%d requests, want 1", m.Count())
	}
}

// TGL-741: every unlisted 4xx is a ValidationError, never retried.
func TestUnlistedClientErrorsAreValidation(t *testing.T) {
	for _, status := range []int{405, 410, 413, 415, 451} {
		m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
			jsonResponse(w, status, `{"statusCode":0,"message":"nope"}`)
		})
		c, _ := newTestClient(t, m.URL)
		_, err := c.Emails.Send(context.Background(), validSend())
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("%d: want ErrValidation, got %v", status, err)
		}
		if m.Count() != 1 {
			t.Fatalf("%d: retried", status)
		}
	}
}

// TGL-741: the error carries the raw body text and the parsed JSON body.
func TestAPIErrorExposesRawAndParsedBody(t *testing.T) {
	const body = `{"statusCode":403,"message":"not allowed","error":"Forbidden"}`
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusForbidden, body)
	})
	c, _ := newTestClient(t, m.URL)
	_, err := c.Emails.Send(context.Background(), validSend())
	apiErr := mustAPIError(t, err)
	if apiErr.RawBody != body {
		t.Fatalf("RawBody = %q", apiErr.RawBody)
	}
	parsed, ok := apiErr.ParsedBody.(map[string]any)
	if !ok || parsed["error"] != "Forbidden" {
		t.Fatalf("ParsedBody = %#v", apiErr.ParsedBody)
	}

	m2 := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html>denied</html>"))
	})
	c2, _ := newTestClient(t, m2.URL)
	_, err = c2.Emails.Send(context.Background(), validSend())
	apiErr = mustAPIError(t, err)
	if apiErr.RawBody != "<html>denied</html>" || apiErr.ParsedBody != nil {
		t.Fatalf("non-JSON body: RawBody=%q ParsedBody=%#v", apiErr.RawBody, apiErr.ParsedBody)
	}
}

// TGL-741: a 2xx that is not JSON is a ServerError carrying the raw text,
// and it is not retried.
func TestNonJSONSuccessIsServerErrorNotRetried(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("<html>ok</html>"))
	})
	c, _ := newTestClient(t, m.URL)
	_, err := c.Emails.Send(context.Background(), validSend())
	if !errors.Is(err, ErrServer) {
		t.Fatalf("want ErrServer, got %v", err)
	}
	if mustAPIError(t, err).RawBody != "<html>ok</html>" {
		t.Fatalf("RawBody = %q", mustAPIError(t, err).RawBody)
	}
	if m.Count() != 1 {
		t.Fatalf("retried %d times", m.Count()-1)
	}
}
