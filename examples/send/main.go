// Command send is the quickstart: one message, one API call.
//
//	export NAIJAMAIL_API_KEY=nmail_live_...
//	go run ./examples/send
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	ncemail "github.com/naijacloud/nc-email-go"
)

func main() {
	// The key is read from NAIJAMAIL_API_KEY. Never hard-code one: a key in
	// source is a key in every clone, every CI log and every backup.
	client, err := ncemail.NewFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	// A deadline of your own, over the SDK's per-attempt timeout. Three
	// attempts with backoff can otherwise outlast a caller's patience.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := client.Emails.Send(ctx, &ncemail.SendEmailRequest{
		From:    "Acme <hello@acme.com>",
		To:      []string{"customer@example.com"},
		Subject: "Your receipt",
		HTML:    "<p>Thanks for your order.</p>",
		Text:    "Thanks for your order.",
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("queued %s (status %s)\n", resp.ID, resp.Status)

	// Always present, never nil. These addresses are on the suppression list
	// and were not mailed; the rest of the message still went.
	for _, r := range resp.Rejected {
		fmt.Printf("rejected %s: %s\n", r.Address, r.Reason)
	}

	// A 202 means queued, not delivered. This is where the state comes from.
	email, err := client.Emails.Get(ctx, resp.ID)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s is %s\n", email.ID, email.Status)
}
