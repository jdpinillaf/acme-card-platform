package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/acme/acme-card/pkg/webhook"
)

// signatureHeader is X-Acme-Signature: "v1=" + hex HMAC-SHA256(secret, ts + "." + body) (pkg/webhook).
func signatureHeader(secret, body []byte, now time.Time) string {
	_, sig := webhook.Sign(secret, body, now)
	return "v1=" + sig
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// blockedIP: loopback, private, link-local (incl. 169.254.169.254 metadata), CGNAT, unspecified, multicast.
func blockedIP(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() ||
		a.IsMulticast() || a.IsInterfaceLocalMulticast() || cgnat.Contains(a)
}

type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

var errRefused = errors.New("partner address refused")

// resolveAllowed returns the address to dial for host: an IP literal or the first resolved one;
// private addresses are refused unless host is a dev-allowed host.
func resolveAllowed(ctx context.Context, host string, allowed []string, lookup lookupFunc) (netip.Addr, error) {
	devHost := slices.Contains(allowed, host)
	addrs := []netip.Addr{}
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = append(addrs, a)
	} else {
		if strings.EqualFold(host, "localhost") && !devHost {
			return netip.Addr{}, fmt.Errorf("%w: %s", errRefused, host)
		}
		var err error
		if addrs, err = lookup(ctx, host); err != nil {
			return netip.Addr{}, err
		}
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("%w: %s does not resolve", errRefused, host)
	}
	if !devHost {
		for _, a := range addrs {
			if blockedIP(a) {
				return netip.Addr{}, fmt.Errorf("%w: %s resolves to %s", errRefused, host, a)
			}
		}
	}
	return addrs[0], nil
}

// ValidateURL is the registration-time check of a partner URL (https only; http and private
// addresses only for dev-allowed hosts; no credentials).
func ValidateURL(ctx context.Context, raw string, allowed []string) error {
	return validateURL(ctx, raw, allowed, defaultLookup)
}

func validateURL(ctx context.Context, raw string, allowed []string, lookup lookupFunc) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("url must be absolute, without credentials")
	}
	host := u.Hostname()
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && slices.Contains(allowed, host):
	default:
		return errors.New("url must use https")
	}
	_, err = resolveAllowed(ctx, host, allowed, lookup)
	return err
}

// NewWebhookClient is the partner HTTP client: it re-checks the address at dial time (a name can
// re-resolve to a private address after registration) and dials exactly the checked IP, sends no
// proxy, follows no redirect, and times out each attempt.
func NewWebhookClient(allowed []string, timeout time.Duration) *http.Client {
	return newWebhookClient(allowed, timeout, defaultLookup)
}

func newWebhookClient(allowed []string, timeout time.Duration, lookup lookupFunc) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ip, err := resolveAllowed(ctx, host, allowed, lookup)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   4,
	}
	return &http.Client{Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
