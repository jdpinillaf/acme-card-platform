package notify

import (
	"context"
	"fmt"
)

// SimProviders stands in for APNs/FCM, an SMS gateway and an email service (dev). A delivered
// message is recorded in the inbox by the delivery loop; fail makes a channel's provider fail
// (NOTIFY_FAIL_CHANNELS, dev only).
type SimProviders struct {
	Fail map[string]bool
}

// Send succeeds unless the channel is set to fail.
func (p SimProviders) Send(_ context.Context, channel, _, _ string) error {
	if p.Fail[channel] {
		return fmt.Errorf("%s provider down (simulated)", channel)
	}
	return nil
}
