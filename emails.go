package ncemail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MessageStatus is a message's delivery state.
//
// A string type rather than an int enum so an unrecognised value the server
// starts sending after this SDK was compiled decodes into a usable value
// instead of failing the whole response. Compare against the constants below;
// use Known to tell a value this SDK understands from one it does not.
//
// The progression is only roughly ordered. A message can go delivered and then
// complained, and providers deliver events out of order often enough that no
// caller should treat this as a state machine.
type MessageStatus string

// The statuses the API sends today.
const (
	StatusQueued     MessageStatus = "queued"
	StatusSent       MessageStatus = "sent"
	StatusDelivered  MessageStatus = "delivered"
	StatusBounced    MessageStatus = "bounced"
	StatusDeferred   MessageStatus = "deferred"
	StatusComplained MessageStatus = "complained"
	StatusRejected   MessageStatus = "rejected"
	StatusFailed     MessageStatus = "failed"
)

// String implements fmt.Stringer.
func (s MessageStatus) String() string { return string(s) }

// Known reports whether the status is one this SDK version was built against.
// A false result is not an error: it means the API has grown a state and this
// SDK predates it.
func (s MessageStatus) Known() bool {
	switch s {
	case StatusQueued, StatusSent, StatusDelivered, StatusBounced,
		StatusDeferred, StatusComplained, StatusRejected, StatusFailed:
		return true
	}
	return false
}

// Attachment is a file to send with a message.
//
// Content is raw bytes; the SDK base64-encodes them on the way out. There is
// deliberately no way to pass a file path: an SDK that opens arbitrary paths
// on the caller's behalf is a local-file-disclosure primitive the moment a
// filename reaches it from an HTTP request. Read the file yourself, so the
// decision about which paths are legitimate stays in your code.
type Attachment struct {
	// Filename is what the recipient sees. It must not contain CR, LF or NUL.
	Filename string
	// Content is the raw file bytes, base64-encoded by MarshalJSON.
	Content []byte
	// ContentType is the MIME type, for example "application/pdf". Optional.
	ContentType string
	// ContentID, when set, lets HTML reference the attachment inline as
	// "cid:<ContentID>".
	ContentID string
}

// attachmentWire is the JSON shape the API expects.
type attachmentWire struct {
	Filename    string `json:"filename"`
	Content     string `json:"content"`
	ContentType string `json:"content_type,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
}

// MarshalJSON encodes Content as standard, padded base64.
//
// Written out rather than left to encoding/json's implicit []byte handling so
// the encoding is visible and pinned: the server decodes strictly, refusing
// anything outside the standard alphabet or a length that is not a multiple of
// four, because a silently mangled invoice is worse than a rejected one.
// URL-safe or unpadded base64 would be rejected by that check.
func (a Attachment) MarshalJSON() ([]byte, error) {
	return json.Marshal(attachmentWire{
		Filename:    a.Filename,
		Content:     base64.StdEncoding.EncodeToString(a.Content),
		ContentType: a.ContentType,
		ContentID:   a.ContentID,
	})
}

// SendEmailRequest is the body of a send.
//
// To, CC, BCC and ReplyTo are slices even though the API also accepts a bare
// string in each position. One shape is simpler to reason about, and the array
// form is valid everywhere the string form is.
type SendEmailRequest struct {
	// From is "Name <a@b.com>" or a bare address. The domain must already be
	// verified for the team the key belongs to.
	From string `json:"from"`
	// To needs at least one address.
	To []string `json:"to"`
	// CC is optional.
	CC []string `json:"cc,omitempty"`
	// BCC is optional.
	BCC []string `json:"bcc,omitempty"`
	// ReplyTo is optional.
	ReplyTo []string `json:"reply_to,omitempty"`
	// Subject is sent even when empty, so the message the API receives says
	// what the caller asked for rather than relying on a server-side default.
	Subject string `json:"subject"`
	// HTML is the HTML body. Send HTML, Text or both.
	HTML string `json:"html,omitempty"`
	// Text is the plain-text body.
	Text string `json:"text,omitempty"`
	// Headers are custom headers, at most MaxCustomHeaders of them. The
	// address and signing headers cannot be overridden; see forbiddenHeaders.
	Headers map[string]string `json:"headers,omitempty"`
	// Attachments are files to send.
	Attachments []Attachment `json:"attachments,omitempty"`
	// Tags are analytics labels, at most MaxTags of them.
	Tags map[string]string `json:"tags,omitempty"`
	// IdempotencyKey deduplicates a send. Leave it empty and the SDK generates
	// one per Send call, which is what makes retrying safe. A value set here is
	// used as-is and never regenerated.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// RejectedRecipient is an address the API refused, today always because it is
// on the suppression list.
type RejectedRecipient struct {
	Address string `json:"address"`
	Reason  string `json:"reason"`
}

// SendEmailResponse is the 202 body.
//
// A 202 means the message passed authorisation, suppression, reputation and
// quota checks and has been queued — not that a mailbox has it. Poll
// EmailsService.Get, or wait for a webhook, for delivery.
type SendEmailResponse struct {
	// ID is our message id, stable across a delivery-backend change. The API
	// writes one row per primary recipient; this is the first of them.
	ID string `json:"id"`
	// Status is the state at queue time, normally StatusQueued.
	Status MessageStatus `json:"status"`
	// Rejected lists addresses we refused. The API omits the field entirely
	// when it is empty; the SDK normalises that to an empty slice so callers
	// can range over it without a nil check, and it is never an error — the
	// rest of the message still went.
	Rejected []RejectedRecipient `json:"rejected"`
}

// Email is one message's current state, as returned by EmailsService.Get.
type Email struct {
	ID string `json:"id"`
	// To is a single address: the API keeps one record per primary recipient,
	// so a three-recipient send produces three of these.
	To      string        `json:"to"`
	From    string        `json:"from"`
	Subject string        `json:"subject"`
	Status  MessageStatus `json:"status"`
	// CreatedAt is when the message was accepted.
	CreatedAt time.Time `json:"created_at"`
	// DeliveredAt is nil until delivery is confirmed.
	DeliveredAt *time.Time `json:"delivered_at"`
	Opened      bool       `json:"opened"`
	Clicked     bool       `json:"clicked"`
	// FailureReason is set only when the message failed.
	FailureReason string `json:"failure_reason,omitempty"`
}

// EmailsService sends and retrieves messages. Reach it through Client.Emails.
type EmailsService struct {
	client *Client
}

// forbiddenHeaders cannot be set as custom headers.
//
// Overriding an address header would sidestep the domain authorisation the
// From address is checked against — a caller could pass a verified From and a
// different To in a header. dkim-signature and received are refused for the
// same reason one level down: they are how a receiver decides the message is
// authentic.
var forbiddenHeaders = map[string]bool{
	"from":           true,
	"to":             true,
	"cc":             true,
	"bcc":            true,
	"subject":        true,
	"dkim-signature": true,
	"received":       true,
}

// Send queues a message.
//
// The returned error is always an *APIError; match it with errors.Is against
// the package sentinels. The request struct is not modified — in particular a
// generated idempotency key is sent as a header only, so the same
// *SendEmailRequest can be reused for a genuinely separate send.
func (s *EmailsService) Send(ctx context.Context, req *SendEmailRequest) (*SendEmailResponse, error) {
	if req == nil {
		return nil, validationError("a send request is required")
	}
	if err := req.validate(); err != nil {
		return nil, err
	}

	// One key per Send call, reused by every attempt of that call. This is the
	// whole reason retrying a send is safe: without it, a response lost to a
	// timeout followed by a retry mails the customer twice, and the caller
	// cannot tell "never arrived" from "arrived, response lost".
	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	if idempotencyKey == "" {
		generated, err := newIdempotencyKey()
		if err != nil {
			return nil, err
		}
		idempotencyKey = generated
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, validationError("could not encode the request: %v", err)
	}
	// Checked after encoding because base64 inflates attachments by a third,
	// and the limit the API enforces is on what goes over the wire.
	if len(body) > MaxPayloadBytes {
		return nil, validationError("message is %d bytes encoded, over the %d byte limit", len(body), MaxPayloadBytes)
	}

	out := &SendEmailResponse{}
	if err := s.client.do(ctx, http.MethodPost, "/v1/emails", body,
		map[string]string{"Idempotency-Key": idempotencyKey}, out); err != nil {
		return nil, err
	}

	if out.Rejected == nil {
		out.Rejected = []RejectedRecipient{}
	}
	return out, nil
}

// Get retrieves one message, scoped to the team the API key belongs to.
//
// An unknown id currently comes back as a 400 rather than a 404 — a known
// quirk of the control plane — so this maps that case to ErrNotFound. Test
// with errors.Is(err, ncemail.ErrNotFound) and it will keep working when the
// server is fixed.
func (s *EmailsService) Get(ctx context.Context, id string) (*Email, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, validationError("a message id is required")
	}
	if err := checkNoControlChars("message id", id); err != nil {
		return nil, err
	}

	out := &Email{}
	// PathEscape, so an id carrying a slash or a dot segment addresses a path
	// element rather than climbing out of /v1/emails.
	if err := s.client.do(ctx, http.MethodGet, "/v1/emails/"+url.PathEscape(id), nil, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// validate applies every check the SDK can make without a round trip.
//
// The server checks all of this too. Doing it here is not redundancy for its
// own sake: a caller gets a specific error naming the field, at the call site,
// instead of a 400 whose text they have to correlate with a request they can
// no longer see.
func (r *SendEmailRequest) validate() error {
	if strings.TrimSpace(r.From) == "" {
		return validationError(`"from" is required`)
	}
	if err := checkNoControlChars("from", r.From); err != nil {
		return err
	}

	if len(r.To) == 0 {
		return validationError(`"to" needs at least one address`)
	}
	for _, group := range []struct {
		field string
		addrs []string
	}{
		{"to", r.To},
		{"cc", r.CC},
		{"bcc", r.BCC},
		{"reply_to", r.ReplyTo},
	} {
		for i, addr := range group.addrs {
			if strings.TrimSpace(addr) == "" {
				return validationError("%s[%d] is empty", group.field, i)
			}
			if err := checkNoControlChars(fmt.Sprintf("%s[%d]", group.field, i), addr); err != nil {
				return err
			}
		}
	}

	if total := len(r.To) + len(r.CC) + len(r.BCC); total > MaxRecipients {
		return validationError("%d recipients across to, cc and bcc, over the limit of %d", total, MaxRecipients)
	}

	if err := checkNoControlChars("subject", r.Subject); err != nil {
		return err
	}

	if len(r.Headers) > MaxCustomHeaders {
		return validationError("%d custom headers, over the limit of %d", len(r.Headers), MaxCustomHeaders)
	}
	for name, value := range r.Headers {
		if strings.TrimSpace(name) == "" {
			return validationError("a custom header name is empty")
		}
		if forbiddenHeaders[strings.ToLower(strings.TrimSpace(name))] {
			return validationError("header %q cannot be overridden", name)
		}
		// A colon in a header name splits it into a name and a value at the
		// far end, which is header injection by another route.
		if strings.Contains(name, ":") {
			return validationError("header name %q must not contain a colon", name)
		}
		if err := checkNoControlChars(fmt.Sprintf("header name %q", name), name); err != nil {
			return err
		}
		if err := checkNoControlChars(fmt.Sprintf("header %q", name), value); err != nil {
			return err
		}
	}

	for i, att := range r.Attachments {
		if strings.TrimSpace(att.Filename) == "" {
			return validationError("attachments[%d] needs a filename", i)
		}
		if err := checkNoControlChars(fmt.Sprintf("attachments[%d] filename", i), att.Filename); err != nil {
			return err
		}
		if len(att.Content) == 0 {
			return validationError("attachments[%d] has no content", i)
		}
	}

	if len(r.Tags) > MaxTags {
		return validationError("%d tags, over the limit of %d", len(r.Tags), MaxTags)
	}
	for key, value := range r.Tags {
		if strings.TrimSpace(key) == "" {
			return validationError("a tag key is empty")
		}
		// The server truncates an over-long tag; the SDK refuses it. A label
		// silently shortened server-side makes the caller's own analytics
		// disagree with the dashboard, with nothing to show why.
		if len(key) > MaxTagKeyLength {
			return validationError("tag key %q is %d characters, over the limit of %d", key, len(key), MaxTagKeyLength)
		}
		if len(value) > MaxTagValueLength {
			return validationError("tag %q has a %d character value, over the limit of %d", key, len(value), MaxTagValueLength)
		}
		if err := checkNoControlChars(fmt.Sprintf("tag %q", key), value); err != nil {
			return err
		}
	}

	if r.IdempotencyKey != "" {
		if err := checkNoControlChars("idempotency key", r.IdempotencyKey); err != nil {
			return err
		}
		if len(r.IdempotencyKey) > MaxIdempotencyKeyLength {
			return validationError("idempotency key is %d characters, over the limit of %d", len(r.IdempotencyKey), MaxIdempotencyKeyLength)
		}
	}

	return nil
}

// checkNoControlChars rejects CR, LF and NUL.
//
// This is the header-injection defence. A newline in an address, a subject or
// a header value ends the header early and lets whatever follows be read as
// further headers — an extra Bcc, a forged Reply-To — by any hop that parses
// the message. The server rejects these too, in MimeBuilder; the SDK rejects
// them so the caller sees a clear local error naming the field.
func checkNoControlChars(field, value string) error {
	if i := strings.IndexAny(value, "\r\n\x00"); i >= 0 {
		return validationError("%s contains a line break or NUL at position %d, which is not allowed", field, i)
	}
	return nil
}
