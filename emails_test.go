package ncemail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSendSuccess(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/emails" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		jsonResponse(w, http.StatusAccepted, `{"id":"5b1e0000-0000-4000-8000-000000000001","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	resp, err := c.Emails.Send(context.Background(), validSend())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.ID != "5b1e0000-0000-4000-8000-000000000001" {
		t.Fatalf("id = %q", resp.ID)
	}
	if resp.Status != StatusQueued {
		t.Fatalf("status = %q", resp.Status)
	}
	// The API omits `rejected` entirely when it is empty. Normalising to an
	// empty slice means callers never branch on absence.
	if resp.Rejected == nil {
		t.Fatal("Rejected is nil; it must always be a usable slice")
	}
	if len(resp.Rejected) != 0 {
		t.Fatalf("Rejected = %v", resp.Rejected)
	}
}

func TestSendWireFormat(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"x","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	req := &SendEmailRequest{
		From:    "Acme <hello@acme.com>",
		To:      []string{"a@example.com", "b@example.com"},
		CC:      []string{"c@example.com"},
		BCC:     []string{"d@example.com"},
		ReplyTo: []string{"support@acme.com"},
		Subject: "",
		Text:    "Attached.",
		Headers: map[string]string{"X-Entity-Ref": "1024"},
		Tags:    map[string]string{"campaign": "invoices"},
		Attachments: []Attachment{{
			Filename:    "invoice-1024.pdf",
			Content:     []byte("%PDF-1.4\n"),
			ContentType: "application/pdf",
			ContentID:   "invoice",
		}},
	}
	if _, err := c.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(m.Requests()[0].Body, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}

	// The API also accepts replyTo, but every SDK sends the snake_case form so
	// a payload captured from one binding reads the same as another's.
	if _, ok := body["reply_to"]; !ok {
		t.Fatalf("reply_to missing from %v", body)
	}
	if _, ok := body["replyTo"]; ok {
		t.Fatal("camelCase replyTo must not be sent")
	}
	// Subject is sent even when empty, so the message says what the caller asked
	// for rather than depending on a server-side default.
	if _, ok := body["subject"]; !ok {
		t.Fatal("subject must be sent even when empty")
	}
	if _, ok := body["to"].([]any); !ok {
		t.Fatalf("to should be an array, got %T", body["to"])
	}

	atts, ok := body["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments = %v", body["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["content"] != base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n")) {
		t.Fatalf("attachment content = %v", att["content"])
	}
	if att["content_type"] != "application/pdf" || att["content_id"] != "invoice" {
		t.Fatalf("attachment wire names wrong: %v", att)
	}
}

func TestAttachmentMarshalOmitsEmptyOptionals(t *testing.T) {
	raw, err := json.Marshal(Attachment{Filename: "a.txt", Content: []byte("hi")})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, "content_type") || strings.Contains(got, "content_id") {
		t.Fatalf("empty optionals should be omitted: %s", got)
	}
	if !strings.Contains(got, `"content":"aGk="`) {
		t.Fatalf("content not standard padded base64: %s", got)
	}
}

func TestSendRejectedPassthrough(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted,
			`{"id":"abc","status":"queued","rejected":[{"address":"x@y.com","reason":"suppressed"}]}`)
	})
	c, _ := newTestClient(t, m.URL)

	resp, err := c.Emails.Send(context.Background(), validSend())
	// A rejected recipient is not an error: the rest of the message still went.
	if err != nil {
		t.Fatalf("rejected recipients must not surface as an error: %v", err)
	}
	if len(resp.Rejected) != 1 {
		t.Fatalf("Rejected = %v", resp.Rejected)
	}
	if resp.Rejected[0].Address != "x@y.com" || resp.Rejected[0].Reason != "suppressed" {
		t.Fatalf("Rejected[0] = %+v", resp.Rejected[0])
	}
}

func TestSendUsesCallerIdempotencyKeyAndLeavesRequestAlone(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	req := validSend()
	req.IdempotencyKey = "order-1024"
	if _, err := c.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := m.Requests()[0].Header.Get("Idempotency-Key"); got != "order-1024" {
		t.Fatalf("Idempotency-Key = %q, want the caller's value", got)
	}
	if req.IdempotencyKey != "order-1024" {
		t.Fatal("the caller's request struct was modified")
	}
}

func TestSendGeneratesIdempotencyKeyWithoutTouchingTheRequest(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	req := validSend()
	if _, err := c.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := m.Requests()[0].Header.Get("Idempotency-Key"); got == "" {
		t.Fatal("no Idempotency-Key was sent")
	}
	// Left empty so the same struct can be reused for a genuinely new send.
	if req.IdempotencyKey != "" {
		t.Fatalf("generated key leaked into the caller's struct: %q", req.IdempotencyKey)
	}
}

func TestGetEmail(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if r.URL.Path != "/v1/emails/5b1e" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "" {
			t.Errorf("GET should not carry an idempotency key, got %q", got)
		}
		jsonResponse(w, http.StatusOK, `{
			"id":"5b1e","to":"x@y.com","from":"hello@acme.com","subject":"Hi",
			"status":"delivered","created_at":"2026-08-29T10:00:00.000Z",
			"delivered_at":"2026-08-29T10:00:04.000Z","opened":false,"clicked":true
		}`)
	})
	c, _ := newTestClient(t, m.URL)

	email, err := c.Emails.Get(context.Background(), "5b1e")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if email.Status != StatusDelivered || email.To != "x@y.com" || !email.Clicked {
		t.Fatalf("email = %+v", email)
	}
	if email.CreatedAt.UTC().Format(time.RFC3339) != "2026-08-29T10:00:00Z" {
		t.Fatalf("created_at = %v", email.CreatedAt)
	}
	if email.DeliveredAt == nil {
		t.Fatal("delivered_at should be set")
	}
	if email.FailureReason != "" {
		t.Fatalf("failure_reason = %q", email.FailureReason)
	}
}

func TestGetEmailUndeliveredHasNilDeliveredAt(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusOK, `{
			"id":"5b1e","to":"x@y.com","from":"h@a.com","subject":"Hi","status":"queued",
			"created_at":"2026-08-29T10:00:00.000Z","delivered_at":null,
			"opened":false,"clicked":false
		}`)
	})
	c, _ := newTestClient(t, m.URL)

	email, err := c.Emails.Get(context.Background(), "5b1e")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if email.DeliveredAt != nil {
		t.Fatalf("delivered_at should be nil, got %v", email.DeliveredAt)
	}
}

// A status this SDK version has never heard of must reach the caller intact
// rather than failing the response.
func TestUnknownStatusPassesThrough(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"quarantined"}`)
	})
	c, _ := newTestClient(t, m.URL)

	resp, err := c.Emails.Send(context.Background(), validSend())
	if err != nil {
		t.Fatalf("an unknown status must not be an error: %v", err)
	}
	if resp.Status != MessageStatus("quarantined") {
		t.Fatalf("status = %q", resp.Status)
	}
	if resp.Status.Known() {
		t.Fatal("Known() should be false for an unrecognised status")
	}
	if !StatusBounced.Known() || StatusBounced.String() != "bounced" {
		t.Fatal("the documented statuses should be Known")
	}
}

func TestGetEmailIDValidation(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusOK, `{}`)
	})
	c, _ := newTestClient(t, m.URL)

	for _, id := range []string{"", "   ", "abc\r\nX: 1"} {
		if _, err := c.Emails.Get(context.Background(), id); !errors.Is(err, ErrValidation) {
			t.Fatalf("id %q: expected ErrValidation, got %v", id, err)
		}
	}
	if m.Count() != 0 {
		t.Fatal("a rejected id must not reach the network")
	}
}

// An id with path syntax in it must address a path element, not climb out of
// /v1/emails.
func TestGetEmailIDIsPathEscaped(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusOK, `{"id":"x","to":"a@b.com","from":"c@d.com","subject":"","status":"queued","created_at":"2026-08-29T10:00:00.000Z","opened":false,"clicked":false}`)
	})
	c, _ := newTestClient(t, m.URL)

	if _, err := c.Emails.Get(context.Background(), "../../admin"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := m.Requests()[0].Path; got != "/v1/emails/../../admin" {
		t.Fatalf("path = %q; the id escaped its path element", got)
	}
}

func TestSendNilRequest(t *testing.T) {
	c, _ := newTestClient(t, "https://api.naijacloud.com")
	if _, err := c.Emails.Send(context.Background(), nil); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

// Header injection: a line break anywhere an address or a header value can
// reach must be refused before the request is built.
func TestHeaderInjectionRejected(t *testing.T) {
	cases := map[string]func(*SendEmailRequest){
		"from CRLF":         func(r *SendEmailRequest) { r.From = "a@b.com\r\nBcc: evil@x.com" },
		"to LF":             func(r *SendEmailRequest) { r.To = []string{"a@b.com\nBcc: evil@x.com"} },
		"cc CR":             func(r *SendEmailRequest) { r.CC = []string{"a@b.com\rX: 1"} },
		"bcc NUL":           func(r *SendEmailRequest) { r.BCC = []string{"a@b.com\x00"} },
		"reply_to LF":       func(r *SendEmailRequest) { r.ReplyTo = []string{"a@b.com\nX: 1"} },
		"subject LF":        func(r *SendEmailRequest) { r.Subject = "Hi\nBcc: evil@x.com" },
		"header name CRLF":  func(r *SendEmailRequest) { r.Headers = map[string]string{"X-A\r\nBcc": "1"} },
		"header value CRLF": func(r *SendEmailRequest) { r.Headers = map[string]string{"X-A": "1\r\nBcc: evil@x.com"} },
		"header name colon": func(r *SendEmailRequest) { r.Headers = map[string]string{"X-A: 1 X-B": "1"} },
		"filename LF":       func(r *SendEmailRequest) { r.Attachments = []Attachment{{Filename: "a\n.pdf", Content: []byte("x")}} },
		"tag value CRLF":    func(r *SendEmailRequest) { r.Tags = map[string]string{"campaign": "a\r\nb"} },
		"idempotency CRLF":  func(r *SendEmailRequest) { r.IdempotencyKey = "a\r\nb" },
	}

	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		t.Error("a request with an injected line break reached the network")
		jsonResponse(w, http.StatusAccepted, `{"id":"x","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := validSend()
			mutate(req)
			_, err := c.Emails.Send(context.Background(), req)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
			// Local failures carry status 0 so one error path covers both
			// local and server rejections.
			if apiErr := mustAPIError(t, err); apiErr.StatusCode != 0 {
				t.Fatalf("StatusCode = %d, want 0", apiErr.StatusCode)
			}
		})
	}
	if m.Count() != 0 {
		t.Fatalf("%d requests reached the server", m.Count())
	}
}

func TestForbiddenHeadersRejected(t *testing.T) {
	c, _ := newTestClient(t, "https://api.naijacloud.com")

	for _, name := range []string{"From", "to", "CC", "Bcc", "Subject", "DKIM-Signature", "received"} {
		req := validSend()
		req.Headers = map[string]string{name: "x"}
		if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("header %q should be refused, got %v", name, err)
		}
	}

	// An ordinary custom header is still allowed.
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"x","status":"queued"}`)
	})
	ok, _ := newTestClient(t, m.URL)
	req := validSend()
	req.Headers = map[string]string{"X-Entity-Ref-ID": "1024"}
	if _, err := ok.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("a normal custom header should be allowed: %v", err)
	}
}

func TestClientSideLimits(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		t.Error("an over-limit request reached the network")
		jsonResponse(w, http.StatusAccepted, `{"id":"x","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	t.Run("recipients", func(t *testing.T) {
		req := validSend()
		req.To = make([]string, 40)
		for i := range req.To {
			req.To[i] = fmt.Sprintf("a%d@example.com", i)
		}
		req.CC = make([]string, 11)
		for i := range req.CC {
			req.CC[i] = fmt.Sprintf("c%d@example.com", i)
		}
		if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("51 recipients should be refused, got %v", err)
		}
	})

	t.Run("headers", func(t *testing.T) {
		req := validSend()
		req.Headers = map[string]string{}
		for i := 0; i <= MaxCustomHeaders; i++ {
			req.Headers[fmt.Sprintf("X-H-%d", i)] = "1"
		}
		if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("26 headers should be refused, got %v", err)
		}
	})

	t.Run("tags", func(t *testing.T) {
		req := validSend()
		req.Tags = map[string]string{}
		for i := 0; i <= MaxTags; i++ {
			req.Tags[fmt.Sprintf("t%d", i)] = "1"
		}
		if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("11 tags should be refused, got %v", err)
		}
	})

	t.Run("tag key and value length", func(t *testing.T) {
		req := validSend()
		req.Tags = map[string]string{strings.Repeat("k", MaxTagKeyLength+1): "1"}
		if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("an over-long tag key should be refused, got %v", err)
		}

		req = validSend()
		req.Tags = map[string]string{"campaign": strings.Repeat("v", MaxTagValueLength+1)}
		if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("an over-long tag value should be refused, got %v", err)
		}
	})

	t.Run("payload size", func(t *testing.T) {
		req := validSend()
		// Measured like the server: html + text + decoded attachment bytes.
		req.Attachments = []Attachment{{Filename: "big.bin", Content: make([]byte, MaxPayloadBytes)}}
		err := func() error {
			_, err := c.Emails.Send(context.Background(), req)
			return err
		}()
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("an oversized payload should be refused, got %v", err)
		}
	})

	t.Run("empty required fields", func(t *testing.T) {
		for name, req := range map[string]*SendEmailRequest{
			"no from":            {To: []string{"a@b.com"}},
			"no to":              {From: "a@b.com"},
			"empty to entry":     {From: "a@b.com", To: []string{"  "}},
			"attachment no name": {From: "a@b.com", To: []string{"b@c.com"}, Attachments: []Attachment{{Content: []byte("x")}}},
			"attachment empty":   {From: "a@b.com", To: []string{"b@c.com"}, Attachments: []Attachment{{Filename: "a.txt"}}},
			"empty tag key":      {From: "a@b.com", To: []string{"b@c.com"}, Tags: map[string]string{"": "1"}},
			"empty header name":  {From: "a@b.com", To: []string{"b@c.com"}, Headers: map[string]string{" ": "1"}},
		} {
			if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
				t.Fatalf("%s: expected ErrValidation, got %v", name, err)
			}
		}
	})

	if m.Count() != 0 {
		t.Fatalf("%d over-limit requests reached the server", m.Count())
	}
}

func TestSendAtExactlyTheRecipientLimit(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"x","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	req := validSend()
	req.To = make([]string, MaxRecipients)
	for i := range req.To {
		req.To[i] = fmt.Sprintf("a%d@example.com", i)
	}
	if _, err := c.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("exactly %d recipients should be accepted: %v", MaxRecipients, err)
	}
}

// A test-key message is never sent, so a "bounced" one is simulated; without
// the flag a caller cannot tell it from a real bounce.
func TestGetEmailExposesSandbox(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"id":"1","to":"x@y.com","from":"h@a.com","subject":"Hi","status":"bounced","created_at":"2026-08-29T10:00:00.000Z","opened":false,"clicked":false,"sandbox":true}`, true},
		{`{"id":"1","to":"x@y.com","from":"h@a.com","subject":"Hi","status":"bounced","created_at":"2026-08-29T10:00:00.000Z","opened":false,"clicked":false}`, false},
	}
	for _, tc := range cases {
		m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
			jsonResponse(w, http.StatusOK, tc.body)
		})
		c, _ := newTestClient(t, m.URL)
		email, err := c.Emails.Get(context.Background(), "1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if email.Sandbox != tc.want {
			t.Fatalf("Sandbox = %v, want %v", email.Sandbox, tc.want)
		}
	}
}

// The server truncates tags by JavaScript's .length (UTF-16 units). Counting
// bytes refused accented tags the server accepts; counting runes would let
// emoji through that the server then shortens.
func TestTagLengthCountsUTF16Units(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"1","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	ok := validSend()
	ok.Tags = map[string]string{strings.Repeat("é", 60): strings.Repeat("ọ", 250)}
	if _, err := c.Emails.Send(context.Background(), ok); err != nil {
		t.Fatalf("accented tags within the limit were refused: %v", err)
	}

	for _, tags := range []map[string]string{
		{strings.Repeat("😀", 33): "v"},
		{"k": strings.Repeat("😀", 129)},
	} {
		req := validSend()
		req.Tags = tags
		if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("over-long emoji tag: err = %v, want ErrValidation", err)
		}
	}
}

// TGL-741: the size limit is measured as the server measures it — decoded
// bytes — so an 8 MiB attachment (about 10.7 MiB once base64'd) is sent, and
// a message of exactly the limit is allowed.
func TestSizeLimitCountsDecodedBytes(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	req := validSend()
	req.HTML = ""
	req.Text = "hi"
	req.Attachments = []Attachment{{Filename: "big.bin", Content: make([]byte, 8*1024*1024)}}
	if _, err := c.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("8 MiB attachment refused: %v", err)
	}

	req.Attachments = []Attachment{{Filename: "exact.bin", Content: make([]byte, MaxPayloadBytes-2)}}
	if _, err := c.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("exactly-at-limit message refused: %v", err)
	}

	req.Attachments = []Attachment{{Filename: "over.bin", Content: make([]byte, MaxPayloadBytes-1)}}
	if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
		t.Fatalf("one byte over the limit: want ErrValidation, got %v", err)
	}
	if m.Count() != 2 {
		t.Fatalf("%d requests reached the server, want 2", m.Count())
	}
}

// TGL-741: the idempotency key travels in the header only, never the body.
func TestIdempotencyKeyIsHeaderOnly(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	req := validSend()
	req.IdempotencyKey = "order-1024"
	if _, err := c.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := m.Requests()[0]
	if got.Header.Get("Idempotency-Key") != "order-1024" {
		t.Fatalf("header = %q", got.Header.Get("Idempotency-Key"))
	}
	if strings.Contains(string(got.Body), "idempotency") || strings.Contains(string(got.Body), "order-1024") {
		t.Fatalf("idempotency key leaked into the body: %s", got.Body)
	}
}

// TGL-741: an empty key means "generate one"; the length limit counts UTF-8
// bytes, so 128 two-byte characters (256 bytes) are over it.
func TestIdempotencyKeyEmptyGeneratesAndLengthIsBytes(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL)

	req := validSend()
	req.IdempotencyKey = ""
	if _, err := c.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(m.Requests()[0].Header.Get("Idempotency-Key")) != 36 {
		t.Fatalf("no generated key: %q", m.Requests()[0].Header.Get("Idempotency-Key"))
	}

	req.IdempotencyKey = strings.Repeat("é", 128) // 256 bytes, 128 characters
	if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
		t.Fatalf("256-byte key: want ErrValidation, got %v", err)
	}
	req.IdempotencyKey = strings.Repeat("é", 127) + "a" // 255 bytes
	if _, err := c.Emails.Send(context.Background(), req); err != nil {
		t.Fatalf("255-byte key refused: %v", err)
	}
}

// TGL-741: content_type and content_id are header material too.
func TestAttachmentContentTypeAndIDInjectionRejected(t *testing.T) {
	c, _ := newTestClient(t, "https://api.naijacloud.com")
	for name, att := range map[string]Attachment{
		"content type CR":  {Filename: "a.pdf", Content: []byte("x"), ContentType: "application/pdf\r\nBcc: x@y.com"},
		"content type NUL": {Filename: "a.pdf", Content: []byte("x"), ContentType: "application/pdf\x00"},
		"content id LF":    {Filename: "a.png", Content: []byte("x"), ContentID: "logo\nX-Evil: 1"},
	} {
		req := validSend()
		req.Attachments = []Attachment{att}
		if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
}

// TGL-741: the forbidden-header check runs on the trimmed name, so padding
// cannot smuggle a Bcc or DKIM-Signature past it.
func TestForbiddenHeaderCheckTrimsName(t *testing.T) {
	c, _ := newTestClient(t, "https://api.naijacloud.com")
	for _, name := range []string{" From", "Bcc\t", " DKIM-Signature ", "\tReceived"} {
		req := validSend()
		req.Headers = map[string]string{name: "x"}
		if _, err := c.Emails.Send(context.Background(), req); !errors.Is(err, ErrValidation) {
			t.Fatalf("%q: want ErrValidation, got %v", name, err)
		}
	}
}

// TGL-741: a 202 with no id is not the API; it is a ServerError, not retried.
func TestSendResponseWithoutIDIsServerError(t *testing.T) {
	for _, body := range []string{`{"status":"queued"}`, `{"id":"","status":"queued"}`, `null`} {
		m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
			jsonResponse(w, http.StatusAccepted, body)
		})
		c, _ := newTestClient(t, m.URL)
		_, err := c.Emails.Send(context.Background(), validSend())
		if !errors.Is(err, ErrServer) {
			t.Fatalf("%s: want ErrServer, got %v", body, err)
		}
		if m.Count() != 1 {
			t.Fatalf("%s: retried %d times", body, m.Count()-1)
		}
	}
}
