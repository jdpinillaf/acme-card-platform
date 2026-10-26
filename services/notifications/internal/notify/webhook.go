package notify

import (
	"net/http"
	"time"
)

// NewWebhookClient is the partner HTTP client (hardened in the delivery task).
func NewWebhookClient(allowedHTTPHosts []string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}
