// banksim simulates the bank's settlement webhook: it signs a payload with HMAC-SHA256 and POSTs it.
//
//	BANK_WEBHOOK_SECRET=... go run ./cmd/banksim --deposit <id> --amount 50000 --currency USD [--status failed] [--ref bank-123] [--repeat 5]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/acme/acme-card/pkg/webhook"
)

func main() {
	url := flag.String("url", "http://localhost:8080/v1/webhooks/bank/deposits", "webhook endpoint")
	deposit := flag.String("deposit", "", "deposit id (required)")
	amount := flag.Int64("amount", 0, "amount in minor units (required)")
	currency := flag.String("currency", "USD", "ISO-4217 currency")
	status := flag.String("status", "settled", "settled | failed")
	ref := flag.String("ref", "", "bank external reference (default: random)")
	repeat := flag.Int("repeat", 1, "send the same webhook N times concurrently (idempotency demo)")
	flag.Parse()

	secret := os.Getenv("BANK_WEBHOOK_SECRET")
	if secret == "" || *deposit == "" || *amount <= 0 {
		fmt.Fprintln(os.Stderr, "BANK_WEBHOOK_SECRET, --deposit and --amount are required")
		os.Exit(2)
	}
	if *ref == "" {
		*ref = "bank-" + uuid.NewString()
	}
	body, _ := json.Marshal(map[string]any{"deposit_id": *deposit, "external_ref": *ref, "amount_minor": *amount, "currency": *currency, "status": *status})

	var wg sync.WaitGroup
	for i := 0; i < *repeat; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts, sig := webhook.Sign([]byte(secret), body, time.Now())
			req, _ := http.NewRequest(http.MethodPost, *url, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(webhook.HeaderTimestamp, ts)
			req.Header.Set(webhook.HeaderSignature, sig)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%d] %v\n", i, err)
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			fmt.Printf("[%d] %d replayed=%s %s\n", i, resp.StatusCode, resp.Header.Get("Idempotency-Replayed"), bytes.TrimSpace(b))
		}(i)
	}
	wg.Wait()
}
