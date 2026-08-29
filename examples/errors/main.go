// Command errors shows how to handle each failure the API can produce.
//
//	export NAIJAMAIL_API_KEY=nmail_live_...
//	go run ./examples/errors
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	ncemail "github.com/naijacloud/nc-email-go"
)

func main() {
	client, err := ncemail.NewFromEnv()
	if err != nil {
		// Construction fails for a missing or malformed key and for a
		// non-https base URL, so a misconfiguration surfaces at start-up
		// rather than as a 401 in production an hour later.
		log.Fatal(err)
	}

	_, err = client.Emails.Send(context.Background(), &ncemail.SendEmailRequest{
		From:    "Acme <hello@unverified-domain.example>",
		To:      []string{"customer@example.com"},
		Subject: "Hello",
		Text:    "Hello.",
	})

	switch {
	case err == nil:
		fmt.Println("sent")

	case errors.Is(err, ncemail.ErrValidation):
		// Also covers everything the SDK rejects locally — a line break in a
		// subject, too many recipients, an oversized payload — which carry
		// StatusCode 0.
		fmt.Println("fix the request:", err)

	case errors.Is(err, ncemail.ErrAuthentication):
		fmt.Println("the API key is missing, revoked or wrong:", err)

	case errors.Is(err, ncemail.ErrPermission):
		// A test key on the live send path, an unverified From domain, a
		// domain paused for reputation, or the daily quota. None of these get
		// better by retrying.
		fmt.Println("not allowed:", err)

	case errors.Is(err, ncemail.ErrRateLimit):
		// The SDK already retried and backed off; reaching here means the
		// limit outlasted the attempts. Shed load rather than spin.
		var apiErr *ncemail.APIError
		if errors.As(err, &apiErr) && apiErr.RateLimit != nil {
			fmt.Printf("rate limited, try again in %v\n", apiErr.RateLimit.RetryAfter)
		}

	case errors.Is(err, ncemail.ErrNotFound):
		fmt.Println("no such message:", err)

	case errors.Is(err, ncemail.ErrTimeout), errors.Is(err, ncemail.ErrConnection):
		// Already retried. The message may or may not have been accepted —
		// but the idempotency key means sending it again later is safe.
		fmt.Println("could not reach the API:", err)

	case errors.Is(err, ncemail.ErrServer):
		fmt.Println("the API failed:", err)

	default:
		fmt.Println("unexpected:", err)
	}

	// Every error carries the request id, which is how support finds the send
	// in our logs. Quote it in a ticket.
	var apiErr *ncemail.APIError
	if errors.As(err, &apiErr) {
		fmt.Printf("status=%d label=%q request_id=%q\n", apiErr.StatusCode, apiErr.ErrorLabel, apiErr.RequestID)
	}

	// A caller-supplied idempotency key survives your own process restarting,
	// which the SDK's per-call key cannot.
	_, _ = client.Emails.Send(context.Background(), &ncemail.SendEmailRequest{
		From:           "Acme <hello@acme.com>",
		To:             []string{"customer@example.com"},
		Subject:        "Order 1024 confirmed",
		Text:           "Thanks.",
		IdempotencyKey: fmt.Sprintf("order-1024-confirmation-%s", time.Now().UTC().Format("2006-01")),
	})
}
