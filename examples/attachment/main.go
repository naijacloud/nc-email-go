// Command attachment sends a message with a file attached.
//
//	export NAIJAMAIL_API_KEY=nmail_live_...
//	go run ./examples/attachment
package main

import (
	"context"
	"fmt"
	"log"

	ncemail "github.com/naijacloud/nc-email-go"
)

func main() {
	client, err := ncemail.NewFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	// Attachment.Content is raw bytes and the SDK base64-encodes them. There
	// is deliberately no "attach this path" helper: reading the file is your
	// decision, so a filename arriving from an HTTP request cannot turn the
	// SDK into a way to read arbitrary files off your server.
	//
	// In real code this would be os.ReadFile of a path you chose, or bytes you
	// generated — an invoice PDF, a CSV export.
	invoice := []byte("%PDF-1.4\n% a real invoice would go here\n")

	resp, err := client.Emails.Send(context.Background(), &ncemail.SendEmailRequest{
		From:    "Acme <billing@acme.com>",
		To:      []string{"customer@example.com"},
		Subject: "Invoice #1024",
		HTML:    `<p>Your invoice is attached.</p>`,
		Attachments: []ncemail.Attachment{{
			Filename:    "invoice-1024.pdf",
			Content:     invoice,
			ContentType: "application/pdf",
		}},
		Tags: map[string]string{"campaign": "invoices", "env": "production"},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("queued %s\n", resp.ID)
}
