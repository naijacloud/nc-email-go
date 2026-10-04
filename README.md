<p align="center">
  <a href="https://www.naijacloud.com">
    <img alt="Naijamail — Go SDK" src="https://raw.githubusercontent.com/naijacloud/nc-email-go/main/.github/assets/banner.png" width="100%">
  </a>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/naijacloud/nc-email-go"><img alt="go" src="https://img.shields.io/badge/go-nc--email--go-008751?style=flat-square&labelColor=0A0E0C"></a>
  <img alt="go" src="https://img.shields.io/badge/go-%3E%3D_1.21-00ADD8?style=flat-square&labelColor=0A0E0C">
  <img alt="dependencies" src="https://img.shields.io/badge/dependencies-0-46C98A?style=flat-square&labelColor=0A0E0C">
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-MIT-8A988F?style=flat-square&labelColor=0A0E0C"></a>
</p>

<p align="center">
  <a href="#quickstart">Quickstart</a> ·
  <a href="#configuration">Configuration</a> ·
  <a href="#errors">Errors</a> ·
  <a href="#retries">Retries</a> ·
  <a href="#webhooks">Webhooks</a> ·
  <a href="#security">Security</a>
</p>

# nc-email-go

The official Go SDK for [Naijamail](https://www.naijacloud.com), the
transactional email API of Naija Cloud.

Zero dependencies — standard library only, including in the tests.

```sh
go get github.com/naijacloud/nc-email-go
```

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"log"

	ncemail "github.com/naijacloud/nc-email-go"
)

func main() {
	client, err := ncemail.NewFromEnv() // reads NAIJAMAIL_API_KEY
	if err != nil {
		log.Fatal(err)
	}

	resp, err := client.Emails.Send(context.Background(), &ncemail.SendEmailRequest{
		From:    "Acme <hello@acme.com>",
		To:      []string{"customer@example.com"},
		Subject: "Your receipt",
		HTML:    "<p>Thanks for your order.</p>",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.ID, resp.Status)
}
```

`Send` returns when the API answers `202`: the message passed authorisation,
suppression, reputation and quota checks and has been queued, not that a
mailbox has it.

```go
email, err := client.Emails.Get(ctx, resp.ID)
fmt.Println(email.Status, email.DeliveredAt) // delivered, 2026-08-29 10:00:04
```

Those are the two endpoints this SDK covers today. The API also has batch send,
a message list, limits, domains and suppressions (see the
[API docs](https://naijacloud.com/docs/api/email)); they are not wrapped here
yet.

Runnable programs are in [`examples/`](examples): [send](examples/send),
[attachment](examples/attachment), [errors](examples/errors),
[webhook](examples/webhook). Each reads `NAIJAMAIL_API_KEY` from the
environment.

## Configuration

```go
client, err := ncemail.New("nmail_live_...",
	ncemail.WithBaseURL("https://api.naijacloud.com"),
	ncemail.WithTimeout(30*time.Second),
	ncemail.WithMaxRetries(2),
	ncemail.WithUserAgentSuffix("acme-billing/2.1"),
	ncemail.WithHTTPClient(myClient),
)
```

| Option | Default | Notes |
| --- | --- | --- |
| `WithBaseURL` | `https://api.naijacloud.com` | Must be `https` unless the host is `localhost`, `127.0.0.1` or `::1`. Also settable with `NAIJAMAIL_BASE_URL`. |
| `WithTimeout` | 30s | Per attempt, not per call. Three attempts can take three times this long unless your `context` cuts it short. |
| `WithMaxRetries` | 2 | Retries after the first attempt, so three attempts in total. |
| `WithUserAgentSuffix` | none | Appended to `nc-email-go/0.1.0 (go/go1.x)`. |
| `WithHTTPClient` | a fresh one | Your client is **copied**, not used directly, so the SDK can set its redirect policy without changing yours. The copy shares your `Transport`. |

### Which key

Two kinds work, and the SDK cannot tell them apart once it has one:

- **`nc_live_…`** — a workspace API key from **Settings → API keys**, ticked for
  the **Email send** scope. Most teams already have one: it is the same
  credential CI deploys with. Add **Platform API** as well if the key also needs
  to manage sending domains or suppressions.
- **`nmail_live_…` / `nmail_test_…`** — a Naijamail-only key from **Email**. The
  test variant is **sandboxed**: the API accepts the send, returns a real id
  and a final status, and never hands the message to a mail server. Use one in
  staging and CI. Send from any domain you have added, or from
  `…@test.mail.naijacloud.dev`; send *to* `delivered@`, `bounced@` or
  `complained@test.mail.naijacloud.dev` to get that outcome. A message sent
  this way comes back from `get` with `sandbox` set to true. There is no test
  variant of a workspace key.

An `nc_pat_…` platform token is not accepted: those predate the Email send scope
and the API refuses them on the mail routes, so the SDK refuses them at
construction rather than a request later.

`New("")` and `NewFromEnv()` both read `NAIJAMAIL_API_KEY`. Construction fails
if the key is missing or malformed, so a bad deploy breaks at start-up rather
than as a 401 an hour later.

Every network method takes a `context.Context` first and honours cancellation,
including during a retry backoff.

## Errors

One error type, `*APIError`, matched against sentinels with `errors.Is`:

```go
resp, err := client.Emails.Send(ctx, req)
switch {
case errors.Is(err, ncemail.ErrPermission):
	// Unverified From domain, a key without the right scope, or the daily quota.
case errors.Is(err, ncemail.ErrRateLimit):
	var apiErr *ncemail.APIError
	errors.As(err, &apiErr)
	log.Printf("retry in %v", apiErr.RateLimit.RetryAfter)
}
```

| Sentinel | When |
| --- | --- |
| `ErrValidation` | 400, 422, and everything the SDK refuses locally |
| `ErrAuthentication` | 401 |
| `ErrPermission` | 403 |
| `ErrNotFound` | 404, and the 400 the server sends for an unknown message id |
| `ErrConflict` | 409 |
| `ErrRateLimit` | 429 |
| `ErrServer` | 5xx, and an unexpected 3xx |
| `ErrConnection` | socket, DNS or TLS failure; a cancelled context |
| `ErrTimeout` | 408, a client-side deadline, an expired context |
| `ErrWebhookVerification` | a signature that does not check out |

`*APIError` carries `Message`, `StatusCode`, `ErrorLabel` (the server's short
label), `RequestID` (from `x-request-id` — quote it in a support ticket) and
the raw `Body`. Failures the SDK catches before sending anything carry
`StatusCode 0`, so one error path covers both local and server rejections.

`Unwrap` returns a slice, so `errors.Is(err, ncemail.ErrConnection)` and
`errors.Is(err, context.Canceled)` both match the same value when you cancelled
deliberately. Match with `errors.Is` rather than peeling the chain with
`errors.Unwrap`, which does not follow multiple errors.

Two API behaviours worth knowing:

- **`Rejected` is never nil.** The API omits the field when it is empty; the
  SDK normalises that to an empty slice. Rejected recipients are on the
  suppression list and were not mailed — the rest of the message still went,
  and it is not an error.
- **An unknown message id answers `400`, not `404`.** The SDK maps that exact
  case to `ErrNotFound`, so `errors.Is(err, ncemail.ErrNotFound)` works today
  and keeps working when the server is fixed.

An unrecognised `MessageStatus` passes through as-is rather than failing the
response; `status.Known()` tells you whether this SDK version knows it.

## Retries

Three attempts by default, on `429`, `408`, any `5xx`, and connection or
timeout failures. Nothing else is retried: a `403` on an unverified domain will
never succeed, and repeating it only delays the error you need to see.

Backoff is exponential with full jitter — `random(0, min(8s, 500ms * 2^n))`.
Full jitter rather than a small random nudge because the point is to break up a
thundering herd: when a rate limit trips for many senders at once, backoffs
that differ only slightly retry in a clump and trip it again.

`Retry-After` overrides that, whether the server sends integer seconds or an
HTTP date, clamped to 60s.

This is safe because **every send carries an idempotency key**. The SDK
generates a UUIDv4 per `Send` call and sends it on all three attempts, so a
lost response followed by a retry cannot double-mail a customer. Set
`SendEmailRequest.IdempotencyKey` yourself when the key must survive your own
process restarting — an order confirmation, say — and the SDK uses it verbatim.

## Webhooks

> **Live.** Naija Cloud delivers these events to endpoints you register, signed
> exactly as below. Two details this verifier already handles: the timestamp is
> taken per delivery *attempt*, so a retry never arrives outside the tolerance
> window; and during a secret rotation the header carries two `v1=` values for
> 24 hours, which is why any match is accepted.

```go
payload, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))

event, err := ncemail.VerifyWebhook(
	payload,
	r.Header.Get(ncemail.WebhookSignatureHeader), // NC-Signature
	os.Getenv("NAIJAMAIL_WEBHOOK_SECRET"),
	0, // 0 means the default 5-minute tolerance
)
if errors.Is(err, ncemail.ErrWebhookVerification) {
	http.Error(w, "invalid signature", http.StatusBadRequest)
	return
}
```

Verify the **raw bytes**, exactly as received. Decoding into a struct and
re-encoding changes key order and number formatting, and the digest no longer
matches. `client.Webhooks.Verify` is the same function on a client.

The header is `NC-Signature: t=<unix seconds>,v1=<hex sha256 hmac>`, signed
over `"<t>.<raw body>"`. Several `v1` values may appear during a secret
rotation and any one matching is enough. The timestamp tolerance is the replay
window: a captured delivery stays valid only until it ages out.

## Security

The full list is in [SECURITY.md](SECURITY.md). In short: the key travels in
the `Authorization` header and nowhere else — not the User-Agent, not error
messages, and not `%v`/`%+v`/`%#v` on a client, which render
`nmail_live_***`. HTTPS is enforced at construction, redirects are never
followed (a followed redirect re-sends `Authorization` to whatever host the
`Location` names), CR/LF/NUL are refused anywhere they could break out of a
header, the address and DKIM headers cannot be overridden, attachments are
bytes rather than paths, and webhook signatures are compared in constant time.

Client-side limits fail fast instead of spending a round trip: 50 recipients
across To/CC/BCC, 10 MiB encoded, 25 custom headers, 10 tags.

Use a `nmail_test_` key in staging and CI. It is sandboxed: sends are recorded
and answered, never delivered, so a staging deploy cannot mail real customers.

Report a vulnerability to **security@naijacloud.com**.

## Development

```sh
go vet ./...
go test -race ./...
gofmt -l .        # must print nothing
```

The tests run a mock API on loopback with `net/http/httptest` and need no
network access. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT. See [LICENSE](LICENSE).
