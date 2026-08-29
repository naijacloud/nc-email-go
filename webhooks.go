package ncemail

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	// WebhookSignatureHeader is the header carrying the signature.
	WebhookSignatureHeader = "NC-Signature"
	// DefaultWebhookTolerance is how far a signature's timestamp may be from
	// now. It is the replay window: an attacker who captures a valid delivery
	// can resend it verbatim until the timestamp ages out, so this is short.
	DefaultWebhookTolerance = 5 * time.Minute
)

// WebhookEvent is a decoded event payload.
//
// Data is left raw so a new event type carrying a shape this SDK version has
// never seen still reaches the caller intact. Unmarshal it into your own
// struct once you have switched on Type.
type WebhookEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// WebhooksService verifies inbound webhook signatures. It holds no state;
// reach it through Client.Webhooks, or call VerifyWebhook directly in a
// handler that has no client to hand.
type WebhooksService struct{}

// Verify checks a webhook signature and returns the decoded event.
//
// Pass the raw request body, exactly as received, and the NC-Signature header.
// tolerance may be zero for DefaultWebhookTolerance.
//
// Note: this scheme is fixed by the SDK contract but the control plane does
// not emit these webhooks yet. It is implemented now so both sides ship
// against the same definition.
func (WebhooksService) Verify(payload []byte, signatureHeader, secret string, tolerance time.Duration) (*WebhookEvent, error) {
	return VerifyWebhook(payload, signatureHeader, secret, tolerance)
}

// VerifyWebhook is Verify without a client. See WebhooksService.Verify.
func VerifyWebhook(payload []byte, signatureHeader, secret string, tolerance time.Duration) (*WebhookEvent, error) {
	if tolerance <= 0 {
		tolerance = DefaultWebhookTolerance
	}
	if secret == "" {
		return nil, webhookError("a signing secret is required")
	}
	if len(payload) == 0 {
		return nil, webhookError("the payload is empty")
	}

	timestamp, signatures, err := parseSignatureHeader(signatureHeader)
	if err != nil {
		return nil, err
	}

	// Checked before the HMAC, because a stale-but-correctly-signed delivery
	// is exactly what a replay looks like and there is no point spending the
	// comparison on it. Absolute difference, so a clock ahead of ours is
	// caught as well as one behind.
	drift := time.Since(time.Unix(timestamp, 0))
	if drift < 0 {
		drift = -drift
	}
	if drift > tolerance {
		return nil, webhookError("signature timestamp is %s away from now, outside the %s tolerance", drift.Round(time.Second), tolerance)
	}

	// Signed over the raw bytes. Re-serialising a decoded object first would
	// change key order, spacing or number formatting and produce a different
	// digest for a payload that is byte-for-byte what the server signed.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	// hmac.Equal is crypto/subtle's constant-time compare: its running time
	// does not depend on how many leading bytes matched, so an attacker cannot
	// discover the signature a byte at a time by timing the responses.
	//
	// Every candidate is compared, with no early exit, so the work does not
	// depend on which key in a rotation was used either. There may be several
	// v1 values precisely because a secret rotation signs with both.
	matched := false
	for _, candidate := range signatures {
		if hmac.Equal([]byte(strings.ToLower(candidate)), []byte(expected)) {
			matched = true
		}
	}
	if !matched {
		// The expected signature is never included. Returning it would hand an
		// attacker the answer to the question they were asking.
		return nil, webhookError("no signature matched")
	}

	event := &WebhookEvent{}
	if err := json.Unmarshal(payload, event); err != nil {
		return nil, webhookError("payload is not valid JSON: %v", err)
	}
	return event, nil
}

// parseSignatureHeader reads "t=<unix seconds>,v1=<hex>[,v1=<hex>]".
//
// Unknown keys are ignored rather than rejected: when a v2 scheme is added the
// server will send both, and a verifier that refused the header outright would
// break on the day of the rollout rather than the day v1 is withdrawn.
func parseSignatureHeader(header string) (int64, []string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, nil, webhookError("missing %s header", WebhookSignatureHeader)
	}

	var timestamp int64
	haveTimestamp := false
	var signatures []string

	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "t":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, nil, webhookError("malformed timestamp in %s", WebhookSignatureHeader)
			}
			timestamp = parsed
			haveTimestamp = true
		case "v1":
			if value != "" {
				signatures = append(signatures, value)
			}
		}
	}

	if !haveTimestamp {
		return 0, nil, webhookError("no timestamp in %s", WebhookSignatureHeader)
	}
	if len(signatures) == 0 {
		return 0, nil, webhookError("no v1 signature in %s", WebhookSignatureHeader)
	}
	return timestamp, signatures, nil
}
