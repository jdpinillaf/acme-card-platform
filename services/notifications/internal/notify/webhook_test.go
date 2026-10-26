package notify

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestSignatureVector(t *testing.T) {
	// same vector as web/lib/notifications.test.ts
	got := signatureHeader([]byte("partner-secret"), []byte(`{"id":"e1"}`), time.Unix(1700000000, 0))
	if got != "v1=56654726859e10150ca6bc5f7720da706cb8b5b10ece6873bf998c470dcfd15c" {
		t.Fatalf("signature %s", got)
	}
}

func fakeLookup(m map[string]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		if a, ok := m[host]; ok {
			return []netip.Addr{netip.MustParseAddr(a)}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
}

func TestValidateURL(t *testing.T) {
	look := fakeLookup(map[string]string{"internal.example": "10.0.0.7", "web": "172.18.0.5"})
	allowed := []string{"web"}
	for raw, ok := range map[string]bool{
		"https://partner.example/hook":     true,
		"http://partner.example/hook":      false,
		"https://10.0.0.1/hook":            false,
		"https://169.254.169.254/latest":   false,
		"https://localhost/hook":           false,
		"https://127.0.0.1:8443/hook":      false,
		"https://[::1]/hook":               false,
		"https://internal.example/hook":    false,
		"http://web:3000/api/partner/hook": true,
		"ftp://partner.example/x":          false,
		"https://user:pw@partner.example/": false,
		"not a url":                        false,
	} {
		err := validateURL(context.Background(), raw, allowed, look)
		if (err == nil) != ok {
			t.Fatalf("%s: err=%v want ok=%v", raw, err, ok)
		}
	}
}

func TestDialRefusesPrivateAfterResolve(t *testing.T) {
	// DNS rebinding: the name resolved to a public address at registration, a private one now.
	c := newWebhookClient(nil, time.Second, fakeLookup(map[string]string{"rebind.example": "10.1.2.3"}))
	_, err := c.Post("https://rebind.example/hook", "application/json", strings.NewReader("{}"))
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("dial to a private address: %v", err)
	}
	if c.CheckRedirect == nil || c.CheckRedirect(&http.Request{}, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects must not be followed")
	}
}
