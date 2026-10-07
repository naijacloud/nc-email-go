# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-10-07

Conformance with the other four Naijamail SDKs (TGL-741).

The first tagged version, so the first one `go get` resolves
(`go get github.com/naijacloud/nc-email-go@v0.3.0`). 0.2.0 and 0.1.0 were
written up here but never tagged, so their entries below ship in this release
too.

### Changed

- **Breaking for webhook handlers that pass `0`:** `VerifyWebhook` /
  `Webhooks.Verify` with a tolerance of `0` is now **strict** (only the current
  second passes) instead of meaning "the default". Every other SDK already
  behaved this way. Pass `ncemail.DefaultWebhookTolerance` for the five-minute
  window. A negative tolerance is now an `ErrValidation` error.
- A base URL with a query string or fragment is **refused** at construction
  instead of being stripped silently.
- `WithTimeout(0)` is refused; it no longer means "no timeout". A caller's
  `http.Client` with no `Timeout` gets the 30s default, so every attempt has a
  deadline.
- `WithMaxRetries` accepts 0 to 10.
- The 10 MiB size check measures the message as the server does — HTML + text
  + raw attachment bytes — rather than the encoded JSON, so attachments of
  ~7.5–10 MiB are no longer refused locally.
- The idempotency key is sent in the `Idempotency-Key` header only; it is no
  longer copied into the JSON body. `IdempotencyKey` now has the `json:"-"` tag.
- An `nc_pat_…` token is refused with a message naming the keys that work.

### Added

- `APIError.RawBody` (the response text) and `APIError.ParsedBody` (the decoded
  JSON, or nil), alongside the existing `Body`.

### Fixed

- A 2xx answer to a send with no `id` is an `ErrServer` error instead of an
  empty response. A 2xx body cut short by the deadline is `ErrTimeout`
  (retryable) rather than a decode error.
- Webhook `t` must be 1–12 ASCII digits; a value near `int64`'s maximum no
  longer reaches the drift arithmetic. A payload that is not a JSON object
  (`null`, an array) is refused rather than returning an empty event.
- Attachment `ContentType` and `ContentID` are checked for CR/LF/NUL.

## 0.2.0 - 2026-10-04 (never published)

Prepared but never tagged. 0.1.0 was not tagged either, so its entries are
folded in here, and both ship in 0.3.0.

### Added

- Accept a workspace API key (`nc_live_…`) alongside the Naijamail keys. It is
  the credential from **Settings → API keys**, and it reaches the mail API when
  it carries the **Email send** scope — so a team that already has one for
  deploys and the platform API does not need a second secret to send mail.
  Redaction knows the new prefix, so a dump still shows which kind of credential
  a process is holding. `nc_pat_…` platform tokens remain refused: they predate
  the scope and the API rejects them on the mail routes.
- `Email.Sandbox` on a retrieved email: true for a message sent with a test key,
  which is recorded but never delivered, so a simulated bounce can be told from
  a real one.

### Fixed

- Tag length is counted in UTF-16 units, the way the server counts it. Byte
  counting refused accented tags (Yoruba, Igbo, French) the server accepts.
- Test keys (`nmail_test_…`) are sandboxed by the API, not refused with a 403.
  The README said otherwise.

## 0.1.0 - 2026-08-29 (never published)

First release. Implements the Naijamail SDK contract for Go.

### Added

- `Client` with functional options: `WithBaseURL`, `WithHTTPClient`,
  `WithTimeout`, `WithMaxRetries`, `WithUserAgentSuffix`. `New` and
  `NewFromEnv` (`NAIJAMAIL_API_KEY`, `NAIJAMAIL_BASE_URL`).
- `client.Emails.Send` (`POST /v1/emails`) and `client.Emails.Get`
  (`GET /v1/emails/{id}`). `SendEmailResponse.Rejected` is always a usable
  slice, never nil.
- One `*APIError` type carrying `Message`, `StatusCode`, `ErrorLabel`,
  `RequestID` and `Body`, matched with `errors.Is` against `ErrValidation`,
  `ErrAuthentication`, `ErrPermission`, `ErrNotFound`, `ErrConflict`,
  `ErrRateLimit`, `ErrServer`, `ErrConnection`, `ErrTimeout` and
  `ErrWebhookVerification`. `RateLimitError` carries `RetryAfter`.
- Retries: three attempts by default, full-jitter exponential backoff,
  `Retry-After` (seconds or HTTP date) honoured and clamped to 60s, and an
  automatic per-call idempotency key so retrying a send cannot double-mail.
- `Webhooks.Verify` / `VerifyWebhook` for `NC-Signature`, with a constant-time
  comparison and a replay window. Naija Cloud emits these events; the scheme
  is shared with every other Naijamail SDK, so all of them verify identically.
- Security: https enforced except on loopback, redirects refused, the API key
  redacted from `String`, `GoString` and `%#v`, header-injection and
  forbidden-header checks, and client-side recipient, payload, header and tag
  limits.
- Zero dependencies, including in tests.

[Unreleased]: https://github.com/naijacloud/nc-email-go/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/naijacloud/nc-email-go/releases/tag/v0.3.0
