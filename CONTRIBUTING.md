# Contributing

## Requirements

Go 1.21 or newer. Nothing else — the module has no dependencies, and neither do
its tests.

## Build and test

```sh
go build ./...
go vet ./...
go test -race ./...
```

The tests run a real HTTP server on loopback with `net/http/httptest`, so they
pass with no network access. They must stay that way: a test that reaches the
internet fails in CI on the day the internet has a bad afternoon, and a test
that needs a live API key cannot run in a fork's pull request at all.

Formatting is checked in CI:

```sh
gofmt -l .   # must print nothing
```

## Where the rules come from

The wire format, the error taxonomy, the retry policy and the security rules
are fixed by `email-sdks/spec/SDK-CONTRACT.md`, which every Naijamail SDK
implements identically. A customer moving from the Node SDK to this one should
rewrite syntax, not behaviour.

If this SDK and the contract disagree, that is a bug here. If the contract and
the control plane disagree, the control plane wins and the contract gets fixed.
Do not change behaviour in this repo alone.

There are exactly two endpoints: `POST /v1/emails` and `GET /v1/emails/{id}`.
Do not add resources the server does not have.

## Changes that need a test

- Anything in the retry loop, the error mapping, or the validation rules.
- Anything touching the API key, redaction, or the redirect policy.

New behaviour without a test against the mock server will not be merged.

## Pull requests

- One change per pull request.
- Update `CHANGELOG.md` under `## [Unreleased]`.
- Exported identifiers need doc comments in the standard Go form.
- Comments explain why. A comment restating the code is noise; a comment naming
  the failure a line prevents is worth keeping.
- Never commit a real key. Tests use the literal
  `nmail_live_test0000000000000000`.
