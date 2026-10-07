// Command webhook is an HTTP handler that verifies Naijamail event signatures.
//
//	export NAIJAMAIL_WEBHOOK_SECRET=nmail_whsec_...
//	go run ./examples/webhook
//
// Note: the control plane does not emit these webhooks yet. The scheme is
// fixed so both sides ship against the same definition; see the README.
package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"

	ncemail "github.com/naijacloud/nc-email-go"
)

func main() {
	secret := os.Getenv("NAIJAMAIL_WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("set NAIJAMAIL_WEBHOOK_SECRET")
	}

	http.HandleFunc("/webhooks/naijamail", func(w http.ResponseWriter, r *http.Request) {
		// The signature covers the bytes as sent. Read them once, here, and do
		// not decode into a struct and re-encode: key order and number
		// formatting would change and the digest would no longer match.
		//
		// MaxBytesReader because an unauthenticated endpoint that reads an
		// unbounded body is a way to exhaust the process's memory.
		payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		event, err := ncemail.VerifyWebhook(payload, r.Header.Get(ncemail.WebhookSignatureHeader), secret, ncemail.DefaultWebhookTolerance)
		if err != nil {
			if errors.Is(err, ncemail.ErrWebhookVerification) {
				// 400, not 500: this is a bad delivery, and answering 5xx
				// would have the sender retry a payload that will never pass.
				http.Error(w, "invalid signature", http.StatusBadRequest)
				return
			}
			http.Error(w, "error", http.StatusInternalServerError)
			return
		}

		// Answer quickly and do the work elsewhere: a sender that times out
		// waiting for you will redeliver, and your handler will run twice.
		log.Printf("event %s type=%s data=%s", event.ID, event.Type, event.Data)
		w.WriteHeader(http.StatusOK)
	})

	log.Fatal(http.ListenAndServe("127.0.0.1:8080", nil))
}
