# Security

## Reporting a vulnerability

Email **security@naijacloud.com**. Do not open a public issue, and do not post
a proof of concept anywhere public until a fix has shipped.

Include what you can: the version, a reproduction, and what an attacker gets
out of it. We acknowledge within two working days and will tell you what we
intend to do and when.

If you believe an API key has leaked — yours or anyone's — revoke it from the
Naijamail dashboard first and mail us second. Revocation is immediate.

## What this SDK guarantees

These are the properties the SDK contract requires of every Naijamail binding.
A change that breaks one is a security bug, not a design decision.

**The key travels in the Authorization header and nowhere else.** It is not in
the User-Agent, not in any error message, and not in `String()`, `GoString()`
or `%#v` output — those render `nmail_live_***`. A `*Client` in a log line or a
panic dump does not take a credential with it.

**HTTPS is enforced at construction.** A base URL whose scheme is not `https`
is refused unless the host is `localhost`, `127.0.0.1` or `::1`, for local
development against a dev control plane. A plaintext base URL would put a live
sending credential on the wire in clear.

**Redirects are never followed.** `CheckRedirect` returns
`http.ErrUseLastResponse` and a 3xx surfaces as `ErrServer`. Go's default is to
follow, re-sending `Authorization` — a DNS takeover or a compromised edge could
answer with a `Location` on a host that is not ours and collect the key.

If you supply your own `*http.Client` through `WithHTTPClient`, the SDK copies
it and sets the redirect policy on the copy. Your client is not modified, and
the copy shares your `Transport`, so pooling still works.

**Every send carries an idempotency key.** Without one, a retry after a lost
response mails a customer twice. The SDK generates a UUIDv4 from `crypto/rand`
once per `Send` call and sends it on all three attempts of that call. A
caller-supplied `IdempotencyKey` wins and is never regenerated.

**Header injection is refused before the request is built.** A CR, LF or NUL in
`From`, any address in `To`/`CC`/`BCC`/`ReplyTo`, `Subject`, any custom header
name or value, any attachment filename, or any tag value is an `ErrValidation`
naming the field. The server checks this too; the SDK checks so the caller gets
a clear local error instead of a 400 from a machine they cannot see.

**Address and signing headers cannot be overridden.** `from`, `to`, `cc`,
`bcc`, `subject`, `dkim-signature` and `received` are refused as custom headers
(case-insensitively), because setting one would sidestep the domain
authorisation the `From` address is checked against.

**Attachments are bytes, never paths.** `Attachment.Content` is `[]byte` and
the SDK base64-encodes it. There is no way to hand the SDK a file path: an SDK
that opens arbitrary paths on the caller's behalf is a local-file-disclosure
primitive the moment a filename reaches it from an HTTP request.

**Webhook signatures are compared in constant time.** `hmac.Equal`, over the
raw request bytes, with a replay window. The error never contains the expected
signature.

**No global mutable state and no dependencies.** A client owns its key, base
URL and HTTP client; two clients with two keys in one process do not interfere.
The module requires nothing outside the Go standard library — including in its
tests — because every third-party dependency is supply-chain risk on a package
that holds a sending credential.

## Handling keys in your own code

Read the key from the environment or a secret manager. A key in source is a key
in every clone, every CI log and every backup. Use a `nmail_test_` key in
staging: the live send path refuses it with a 403, so a staging deploy holding
production credentials fails loudly instead of mailing real customers.
