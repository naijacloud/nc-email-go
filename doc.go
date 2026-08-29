// Package ncemail is the official Go SDK for Naijamail, the transactional
// email API of Naija Cloud.
//
// # Quickstart
//
//	client, err := ncemail.NewFromEnv() // reads NAIJAMAIL_API_KEY
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	resp, err := client.Emails.Send(context.Background(), &ncemail.SendEmailRequest{
//		From:    "Acme <hello@acme.com>",
//		To:      []string{"customer@example.com"},
//		Subject: "Your receipt",
//		HTML:    "<p>Thanks for your order.</p>",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	fmt.Println(resp.ID, resp.Status)
//
// A send returns 202: the message passed authorisation, suppression,
// reputation and quota checks and has been queued, not that a mailbox has it.
// Poll [EmailsService.Get] for the current state.
//
// # Errors
//
// Every method returns an [*APIError]. Match it against the package sentinels:
//
//	if errors.Is(err, ncemail.ErrRateLimit) {
//		var apiErr *ncemail.APIError
//		errors.As(err, &apiErr)
//		time.Sleep(apiErr.RateLimit.RetryAfter)
//	}
//
// Failures the SDK catches before sending anything — a line break in a
// subject, too many recipients, an oversized payload — are [ErrValidation]
// with StatusCode 0, so one error path covers both local and server rejections.
//
// # Retries
//
// Three attempts by default with full-jitter exponential backoff, on 429, 408,
// any 5xx and connection or timeout failures, and on nothing else. Retrying a
// send is safe because every send carries an idempotency key: the SDK
// generates one per Send call and reuses it across that call's attempts.
//
// # Security
//
// The base URL must be https unless it is loopback, redirects are never
// followed, and the API key appears only in the Authorization header — never
// in the User-Agent, an error message, or the client's own String, GoString or
// %#v output. See SECURITY.md.
package ncemail
