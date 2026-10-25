// Package clock abstracts time so lifecycle rules (hold expiry, reversal windows, velocity)
// can be tested deterministically.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time.
type Clock interface{ Now() time.Time }

// System is the real wall clock (UTC).
type System struct{}

// Now returns time.Now in UTC.
func (System) Now() time.Time { return time.Now().UTC() }

// Fake is a manually advanced clock for tests. Safe for concurrent use.
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake returns a Fake set to t.
func NewFake(t time.Time) *Fake { return &Fake{t: t} }

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance moves the fake time forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}
