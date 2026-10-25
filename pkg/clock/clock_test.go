package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFakeAdvance(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewFake(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("Now=%v want %v", c.Now(), t0)
	}
	c.Advance(72 * time.Hour)
	if !c.Now().Equal(t0.Add(72 * time.Hour)) {
		t.Fatalf("after advance Now=%v", c.Now())
	}
}

func TestFakeIsGoroutineSafe(t *testing.T) {
	c := NewFake(time.Unix(0, 0))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c.Advance(time.Second) }()
		go func() { defer wg.Done(); _ = c.Now() }()
	}
	wg.Wait()
	if c.Now() != time.Unix(50, 0) {
		t.Fatalf("Now=%v want 50s", c.Now())
	}
}

func TestSystemIsUTCNow(t *testing.T) {
	if d := time.Since(System{}.Now()); d < 0 || d > time.Second {
		t.Fatalf("System clock off by %v", d)
	}
}
