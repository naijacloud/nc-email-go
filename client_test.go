package ncemail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsBadKeyShape(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"no prefix", "sk_live_abcdefgh"},
		{"wrong environment", "nmail_prod_abcdefgh"},
		{"too short", "nmail_live_short"},
		{"illegal character", "nmail_live_abcdefg!"},
		{"whitespace only", "   "},
		// The pre-scopes platform token. The API refuses it on the mail routes
		// outright — it predates the Email send scope and was never granted
		// mail access — so it fails here rather than at send time.
		{"platform token", "nc_pat_0123456789abcdef"},
		// There is no test variant of a workspace key.
		{"workspace test variant", "nc_test_0123456789abcdef"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Cleared so a developer's own key in the environment cannot make
			// the empty-key case pass by accident.
			t.Setenv(EnvAPIKey, "")

			_, err := New(tc.key)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
			// A rejected key must not be echoed: the error text ends up in
			// logs, and a typo'd key is still nearly a real one.
			if trimmed := strings.TrimSpace(tc.key); trimmed != "" && strings.Contains(err.Error(), trimmed) {
				t.Fatalf("error quoted the key back: %v", err)
			}
		})
	}
}

func TestNewAcceptsLiveAndTestKeys(t *testing.T) {
	for _, key := range []string{testAPIKey, "nmail_test_0000abcd-EFGH_1234"} {
		if _, err := New(key); err != nil {
			t.Fatalf("New(%q): %v", redactKey(key), err)
		}
	}
}

// A workspace API key from Settings -> API keys, carrying the Email send scope.
func TestNewAcceptsWorkspaceKey(t *testing.T) {
	const key = "nc_live_0123456789abcdefghij"

	c, err := New(key)
	if err != nil {
		t.Fatalf("New(%q): %v", redactKey(key), err)
	}
	// Redaction has to know the prefix too, or a workspace key falls through
	// to a bare "***" and an operator loses the one useful signal in a dump:
	// which kind of credential this process is holding.
	if got := redactKey(key); got != keyPrefixWorkspace+"***" {
		t.Fatalf("redactKey = %q, want %q", got, keyPrefixWorkspace+"***")
	}
	if strings.Contains(fmt.Sprintf("%+v", c), "0123456789abcdefghij") {
		t.Fatal("the key leaked into the client's rendering")
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv(EnvAPIKey, testAPIKey)
	c, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if c.BaseURL() != DefaultBaseURL {
		t.Fatalf("base URL = %q, want %q", c.BaseURL(), DefaultBaseURL)
	}

	t.Setenv(EnvAPIKey, "")
	_, err = NewFromEnv()
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if !strings.Contains(err.Error(), EnvAPIKey) {
		t.Fatalf("error should name %s, got %v", EnvAPIKey, err)
	}
}

func TestBaseURLFromEnvironment(t *testing.T) {
	t.Setenv(EnvBaseURL, "https://api.staging.example.com/")
	c, err := New(testAPIKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The trailing slash is trimmed so paths do not end up doubled.
	if c.BaseURL() != "https://api.staging.example.com" {
		t.Fatalf("base URL = %q", c.BaseURL())
	}
}

func TestHTTPSEnforcement(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"https://api.naijacloud.com", true},
		{"https://api.naijacloud.com/proxy", true},
		{"http://api.naijacloud.com", false},
		{"http://example.com:8080", false},
		{"http://localhost:3000", true},
		{"http://127.0.0.1:3000", true},
		{"http://[::1]:3000", true},
		{"https://localhost:3000", true},
		{"ftp://api.naijacloud.com", false},
		{"api.naijacloud.com", false},
	}

	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			_, err := New(testAPIKey, WithBaseURL(tc.url))
			if tc.ok && err != nil {
				t.Fatalf("expected %q to be accepted, got %v", tc.url, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("expected %q to be rejected", tc.url)
				}
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("expected ErrValidation, got %v", err)
				}
			}
		})
	}
}

func TestBaseURLDropsQueryAndFragment(t *testing.T) {
	c, err := New(testAPIKey, WithBaseURL("https://api.naijacloud.com/?token=leaked#frag"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if strings.ContainsAny(c.BaseURL(), "?#") {
		t.Fatalf("base URL kept a query or fragment: %q", c.BaseURL())
	}
}

// The key must not survive any of fmt's verbs, because a client dropped into a
// log line or a panic dump is the easiest way for a credential to escape.
func TestClientStringRedactsKey(t *testing.T) {
	c, err := New(testAPIKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rendered := fmt.Sprintf("%v %+v %#v %s", c, c, c, c)
	for _, secret := range []string{testAPIKey, testKeySecret} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("rendered client leaked the key: %s", rendered)
		}
	}
	if !strings.Contains(rendered, keyPrefixLive+"***") {
		t.Fatalf("expected a redacted key marker, got %s", rendered)
	}
}

func TestRedactKey(t *testing.T) {
	cases := map[string]string{
		testAPIKey:                  "nmail_live_***",
		"nmail_test_abcdefghijklmn": "nmail_test_***",
		"garbage":                   "***",
		"":                          "<none>",
	}
	for in, want := range cases {
		if got := redactKey(in); got != want {
			t.Fatalf("redactKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUserAgentAndAuthHeaders(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"abc","status":"queued"}`)
	})
	c, _ := newTestClient(t, m.URL, WithUserAgentSuffix("acme-billing/2.1"))

	if _, err := c.Emails.Send(context.Background(), validSend()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	req := m.Requests()[0]
	if got := req.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
		t.Fatalf("Authorization = %q", got)
	}
	ua := req.Header.Get("User-Agent")
	if !strings.HasPrefix(ua, "nc-email-go/"+Version+" (go/") {
		t.Fatalf("User-Agent = %q", ua)
	}
	if !strings.HasSuffix(ua, "acme-billing/2.1") {
		t.Fatalf("User-Agent lost the suffix: %q", ua)
	}
	if strings.Contains(ua, testKeySecret) {
		t.Fatalf("User-Agent leaked the key: %q", ua)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestUserAgentSuffixRejectsInjectionAndKeys(t *testing.T) {
	for _, suffix := range []string{"acme\r\nX-Evil: 1", "app " + testAPIKey} {
		if _, err := New(testAPIKey, WithUserAgentSuffix(suffix)); !errors.Is(err, ErrValidation) {
			t.Fatalf("expected ErrValidation for %q, got %v", suffix, err)
		}
	}
}

// A caller's *http.Client is shared with the rest of their program, so the SDK
// copies it instead of reaching in and changing its redirect policy.
func TestWithHTTPClientIsCopiedNotMutated(t *testing.T) {
	caller := &http.Client{Timeout: 12 * time.Second}

	c, err := New(testAPIKey, WithHTTPClient(caller))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if caller.CheckRedirect != nil {
		t.Fatal("the caller's http.Client was mutated")
	}
	if c.httpClient == caller {
		t.Fatal("the SDK used the caller's http.Client directly")
	}
	if c.httpClient.CheckRedirect == nil {
		t.Fatal("the copy has no redirect policy")
	}
	// Without WithTimeout the caller's own timeout is respected.
	if c.httpClient.Timeout != 12*time.Second {
		t.Fatalf("timeout = %v, want 12s", c.httpClient.Timeout)
	}

	c2, err := New(testAPIKey, WithHTTPClient(caller), WithTimeout(3*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c2.httpClient.Timeout != 3*time.Second {
		t.Fatalf("WithTimeout did not override: %v", c2.httpClient.Timeout)
	}
	if caller.Timeout != 12*time.Second {
		t.Fatal("WithTimeout changed the caller's client")
	}
}

func TestOptionValidation(t *testing.T) {
	cases := map[string]Option{
		"empty base URL":   WithBaseURL("  "),
		"nil http client":  WithHTTPClient(nil),
		"negative timeout": WithTimeout(-time.Second),
		"negative retries": WithMaxRetries(-1),
	}
	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(testAPIKey, opt); !errors.Is(err, ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
		})
	}
}

func TestNilContextRejected(t *testing.T) {
	c, _ := newTestClient(t, "https://api.naijacloud.com")
	// Deliberately nil: a caller who forgets the context should get a named
	// error, not a panic from deep inside net/http.
	_, err := c.Emails.Send(nil, validSend()) //nolint:staticcheck
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

// Two clients with two keys in one process must not interfere: nothing about a
// client lives in a package-level variable.
func TestClientsAreIndependent(t *testing.T) {
	m := newMockAPI(t, func(w http.ResponseWriter, r *http.Request, call int) {
		jsonResponse(w, http.StatusAccepted, `{"id":"`+r.Header.Get("Authorization")+`","status":"queued"}`)
	})

	a, _ := newTestClient(t, m.URL)
	b, err := New("nmail_test_00000000000000000000", WithBaseURL(m.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	respA, err := a.Emails.Send(context.Background(), validSend())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	respB, err := b.Emails.Send(context.Background(), validSend())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if respA.ID == respB.ID {
		t.Fatal("both clients sent the same credential")
	}
}

func TestNewIdempotencyKeyShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 128; i++ {
		key, err := newIdempotencyKey()
		if err != nil {
			t.Fatalf("newIdempotencyKey: %v", err)
		}
		if len(key) != 36 {
			t.Fatalf("length = %d, want 36 (%q)", len(key), key)
		}
		if key[14] != '4' {
			t.Fatalf("not a v4 UUID: %q", key)
		}
		if !strings.ContainsAny(string(key[19]), "89ab") {
			t.Fatalf("bad RFC 4122 variant: %q", key)
		}
		if seen[key] {
			t.Fatalf("duplicate key %q after %d draws", key, i)
		}
		seen[key] = true
	}
}
