package audit

import (
	"testing"
	"time"
)

func TestRetentionCutoffIsThirteenCalendarMonths(t *testing.T) {
	now := time.Date(2027, 12, 1, 0, 0, 0, 0, time.UTC)
	if got := (Config{}).withDefaults().cutoff(now); !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("cutoff %v", got)
	}
	if got := (Config{Retention: time.Hour}).withDefaults().cutoff(now); !got.Equal(now.Add(-time.Hour)) {
		t.Fatalf("explicit retention cutoff %v", got)
	}
}
