package ncemail

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const testWebhookSecret = "nmail_whsec_test0000000000000000"

func signPayload(t *testing.T, payload []byte, secret string, at time.Time) string {
	t.Helper()
	ts := at.Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", ts)))
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestWebhookVerifyAccepts(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"email.delivered","created_at":"2026-08-29T10:00:00.000Z","data":{"email_id":"5b1e"}}`)
	header := signPayload(t, payload, testWebhookSecret, time.Now())

	event, err := VerifyWebhook(payload, header, testWebhookSecret, DefaultWebhookTolerance)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if event.ID != "evt_1" || event.Type != "email.delivered" {
		t.Fatalf("event = %+v", event)
	}
	if !strings.Contains(string(event.Data), "5b1e") {
		t.Fatalf("data = %s", event.Data)
	}
	if event.CreatedAt.UTC().Format(time.RFC3339) != "2026-08-29T10:00:00Z" {
		t.Fatalf("created_at = %v", event.CreatedAt)
	}
}

func TestWebhookVerifyThroughClient(t *testing.T) {
	c, _ := newTestClient(t, "https://api.naijacloud.com")
	payload := []byte(`{"id":"evt_1","type":"email.opened"}`)
	header := signPayload(t, payload, testWebhookSecret, time.Now())

	if _, err := c.Webhooks.Verify(payload, header, testWebhookSecret, DefaultWebhookTolerance); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestWebhookVerifyRejectsBadSignature(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"email.delivered"}`)

	cases := map[string]string{
		"wrong secret":     signPayload(t, payload, "nmail_whsec_someoneelse0000", time.Now()),
		"tampered payload": signPayload(t, []byte(`{"id":"evt_2"}`), testWebhookSecret, time.Now()),
		"garbage hex":      fmt.Sprintf("t=%d,v1=deadbeef", time.Now().Unix()),
		"not hex at all":   fmt.Sprintf("t=%d,v1=zzzz", time.Now().Unix()),
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := VerifyWebhook(payload, header, testWebhookSecret, DefaultWebhookTolerance)
			if !errors.Is(err, ErrWebhookVerification) {
				t.Fatalf("want ErrWebhookVerification, got %v", err)
			}
			// Returning the expected signature would hand an attacker the
			// answer to the question they were asking.
			expected := signPayload(t, payload, testWebhookSecret, time.Now())
			sig := expected[strings.Index(expected, "v1=")+3:]
			if strings.Contains(err.Error(), sig) {
				t.Fatalf("the error leaked the expected signature: %v", err)
			}
			if strings.Contains(err.Error(), testWebhookSecret) {
				t.Fatalf("the error leaked the secret: %v", err)
			}
		})
	}
}

// The timestamp check is the replay defence: a captured delivery stays valid
// only until it ages out of the window.
func TestWebhookVerifyRejectsStaleTimestamp(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"email.delivered"}`)

	old := signPayload(t, payload, testWebhookSecret, time.Now().Add(-10*time.Minute))
	if _, err := VerifyWebhook(payload, old, testWebhookSecret, DefaultWebhookTolerance); !errors.Is(err, ErrWebhookVerification) {
		t.Fatalf("a 10-minute-old signature should be refused, got %v", err)
	}

	// A wider tolerance accepts the same delivery, proving it was the age and
	// not the signature that failed.
	if _, err := VerifyWebhook(payload, old, testWebhookSecret, 30*time.Minute); err != nil {
		t.Fatalf("within tolerance it should verify: %v", err)
	}

	// A clock ahead of ours is caught too, not only one behind.
	future := signPayload(t, payload, testWebhookSecret, time.Now().Add(10*time.Minute))
	if _, err := VerifyWebhook(payload, future, testWebhookSecret, DefaultWebhookTolerance); !errors.Is(err, ErrWebhookVerification) {
		t.Fatalf("a future signature should be refused, got %v", err)
	}
}

// During a secret rotation the server signs with both secrets and sends two
// v1 values; either one verifying is enough.
func TestWebhookVerifyAcceptsAnyOfSeveralSignatures(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"email.delivered"}`)
	good := signPayload(t, payload, testWebhookSecret, time.Now())
	ts := good[:strings.Index(good, ",")]
	goodSig := good[strings.Index(good, "v1=")+3:]

	header := ts + ",v1=" + strings.Repeat("0", 64) + ",v1=" + goodSig
	if _, err := VerifyWebhook(payload, header, testWebhookSecret, DefaultWebhookTolerance); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// Signatures are hex-encoded lowercase, but a sender that uppercases them
// should still verify rather than fail confusingly.
func TestWebhookVerifyIsCaseInsensitiveOnHex(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	header := strings.ToUpper(signPayload(t, payload, testWebhookSecret, time.Now()))
	// Only the signature half is uppercased; the keys must stay as they are.
	header = strings.Replace(header, "T=", "t=", 1)
	header = strings.Replace(header, "V1=", "v1=", 1)

	if _, err := VerifyWebhook(payload, header, testWebhookSecret, DefaultWebhookTolerance); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestWebhookVerifyRejectsMalformedInput(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	now := time.Now().Unix()

	cases := map[string]struct {
		payload []byte
		header  string
		secret  string
	}{
		"missing header":    {payload, "", testWebhookSecret},
		"no timestamp":      {payload, "v1=abc", testWebhookSecret},
		"no signature":      {payload, fmt.Sprintf("t=%d", now), testWebhookSecret},
		"empty signature":   {payload, fmt.Sprintf("t=%d,v1=", now), testWebhookSecret},
		"bad timestamp":     {payload, "t=not-a-number,v1=abc", testWebhookSecret},
		"empty payload":     {nil, fmt.Sprintf("t=%d,v1=abc", now), testWebhookSecret},
		"empty secret":      {payload, fmt.Sprintf("t=%d,v1=abc", now), ""},
		"junk header parts": {payload, "nonsense", testWebhookSecret},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyWebhook(tc.payload, tc.header, tc.secret, DefaultWebhookTolerance); !errors.Is(err, ErrWebhookVerification) {
				t.Fatalf("want ErrWebhookVerification, got %v", err)
			}
		})
	}
}

// A future v2 scheme will be sent alongside v1 during the rollout. Ignoring
// keys we do not know means this verifier keeps working on that day.
func TestWebhookVerifyIgnoresUnknownHeaderKeys(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	header := signPayload(t, payload, testWebhookSecret, time.Now()) + ",v2=abcdef,scheme=whatever"

	if _, err := VerifyWebhook(payload, header, testWebhookSecret, DefaultWebhookTolerance); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A signature that checks out over a body that is not JSON is still a failure,
// but the caller must not be handed a half-built event.
func TestWebhookVerifyRejectsNonJSONPayload(t *testing.T) {
	payload := []byte(`not json`)
	header := signPayload(t, payload, testWebhookSecret, time.Now())

	event, err := VerifyWebhook(payload, header, testWebhookSecret, DefaultWebhookTolerance)
	if !errors.Is(err, ErrWebhookVerification) {
		t.Fatalf("want ErrWebhookVerification, got %v", err)
	}
	if event != nil {
		t.Fatalf("event should be nil, got %+v", event)
	}
}

// A single flipped byte anywhere in the body must break the digest.
func TestWebhookVerifyDetectsSingleByteTampering(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"email.delivered","data":{"amount":100}}`)
	header := signPayload(t, payload, testWebhookSecret, time.Now())

	tampered := make([]byte, len(payload))
	copy(tampered, payload)
	tampered[len(tampered)-3] = '9'

	if _, err := VerifyWebhook(tampered, header, testWebhookSecret, DefaultWebhookTolerance); !errors.Is(err, ErrWebhookVerification) {
		t.Fatalf("tampering was not detected: %v", err)
	}
}

// TGL-741: a tolerance of zero is strict, not "use the default".
func TestWebhookZeroToleranceIsStrict(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"email.delivered"}`)
	old := signPayload(t, payload, testWebhookSecret, time.Now().Add(-10*time.Second))
	if _, err := VerifyWebhook(payload, old, testWebhookSecret, 0); !errors.Is(err, ErrWebhookVerification) {
		t.Fatalf("10s old with tolerance 0: want ErrWebhookVerification, got %v", err)
	}
	if _, err := VerifyWebhook(payload, old, testWebhookSecret, DefaultWebhookTolerance); err != nil {
		t.Fatalf("10s old with the default tolerance: %v", err)
	}
}

func TestWebhookNegativeToleranceIsValidationError(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	header := signPayload(t, payload, testWebhookSecret, time.Now())
	if _, err := VerifyWebhook(payload, header, testWebhookSecret, -time.Second); !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}

// TGL-741: t is 1-12 ASCII digits; anything else, including values near
// int64's maximum that once overflowed the drift arithmetic, is refused.
func TestWebhookTimestampIsStrictDigits(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	sig := strings.Repeat("a", 64)
	for _, ts := range []string{
		"+1756468800", "-1756468800", "1_756_468_800", "1756468800.0", "1e9", " ", "",
		"9223372036854775807", "9223372036854775806", "1234567890123",
	} {
		header := "t=" + ts + ",v1=" + sig
		if _, err := VerifyWebhook(payload, header, testWebhookSecret, DefaultWebhookTolerance); !errors.Is(err, ErrWebhookVerification) {
			t.Fatalf("t=%q: want ErrWebhookVerification, got %v", ts, err)
		}
	}
	// 12 digits is accepted by the parser and then fails only on age.
	_, err := VerifyWebhook(payload, "t=999999999999,v1="+sig, testWebhookSecret, DefaultWebhookTolerance)
	if !errors.Is(err, ErrWebhookVerification) || !strings.Contains(err.Error(), "tolerance") {
		t.Fatalf("12-digit t: want a tolerance failure, got %v", err)
	}
}

// TGL-741: only a JSON object is an event.
func TestWebhookRejectsNonObjectPayload(t *testing.T) {
	for _, body := range []string{`[]`, `[{"id":"evt_1"}]`, `"evt"`, `null`, `42`} {
		payload := []byte(body)
		header := signPayload(t, payload, testWebhookSecret, time.Now())
		if _, err := VerifyWebhook(payload, header, testWebhookSecret, DefaultWebhookTolerance); !errors.Is(err, ErrWebhookVerification) {
			t.Fatalf("%s: want ErrWebhookVerification, got %v", body, err)
		}
	}
}
