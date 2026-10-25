//go:build integration

package integration

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// stepRecord is one measured step of the journey: an HTTP call or an asynchronous delivery.
type stepRecord struct {
	Phase, Name string
	Kind        string // "http" or "async"
	Status      int    // HTTP status; 0 for async
	Dur         time.Duration
}

// journal records timings for the customer journey. It never asserts on them (v1 reports only).
type journal struct {
	s      *stack
	phase  string
	steps  []stepRecord
	onResp func(name string, r resp) // called with every step's response (e.g. leak scan)
}

func newJournal(s *stack) *journal { return &journal{s: s} }

func (j *journal) begin(phase string) { j.phase = phase }

// step runs f and records its wall time and status under the current phase.
func (j *journal) step(name string, f func() resp) resp {
	start := time.Now()
	r := f()
	j.steps = append(j.steps, stepRecord{Phase: j.phase, Name: name, Kind: "http", Status: r.status, Dur: time.Since(start)})
	if j.onResp != nil {
		j.onResp(name, r)
	}
	return r
}

// until polls ready every 20 ms and records how long it took to become true. On timeout it
// records and returns -1; the caller decides whether that is a failure.
func (j *journal) until(name string, deadline time.Duration, ready func() bool) time.Duration {
	start := time.Now()
	for {
		if ready() {
			d := time.Since(start)
			j.steps = append(j.steps, stepRecord{Phase: j.phase, Name: name, Kind: "async", Dur: d})
			return d
		}
		if time.Since(start) > deadline {
			j.steps = append(j.steps, stepRecord{Phase: j.phase, Name: name, Kind: "async", Dur: -1})
			return -1
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// audit waits (30 s) for an audit event of eventType about id (as aggregate_id or inside the
// after payload) and records the delivery time.
func (j *journal) audit(t *testing.T, name, eventType, aggregateID string) time.Duration {
	t.Helper()
	d := j.until(name, 30*time.Second, func() bool {
		return count(t, j.s.audit, `SELECT count(*) FROM audit_events WHERE event_type=$1 AND (aggregate_id=$2 OR after::text LIKE '%'||$2||'%')`, eventType, aggregateID) > 0
	})
	if d < 0 {
		t.Fatalf("%s: no %s event for %s in audit after 30s", name, eventType, aggregateID)
	}
	return d
}

func (j *journal) markdown() string {
	var b strings.Builder
	b.WriteString("| Phase | Step | Kind | Status | ms |\n|---|---|---|---|---|\n")
	var http []float64
	for _, s := range j.steps {
		status, ms := "-", "timeout"
		if s.Kind == "http" {
			status = fmt.Sprint(s.Status)
			http = append(http, float64(s.Dur.Milliseconds()))
		}
		if s.Dur >= 0 {
			ms = fmt.Sprint(s.Dur.Milliseconds())
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", s.Phase, s.Name, s.Kind, status, ms)
	}
	if len(http) > 0 {
		sort.Float64s(http)
		fmt.Fprintf(&b, "\nHTTP steps: %d, p50 %.0f ms, p95 %.0f ms, max %.0f ms\n", len(http), pct(http, 50), pct(http, 95), http[len(http)-1])
	}
	return b.String()
}

// pct is the nearest-rank percentile of sorted values.
func pct(sorted []float64, p int) float64 {
	i := (len(sorted)*p + 99) / 100
	if i < 1 {
		i = 1
	}
	return sorted[i-1]
}

// report logs the table and, when JOURNEY_REPORT is set, writes it to that path.
func (j *journal) report(t *testing.T) {
	t.Helper()
	md := j.markdown()
	t.Logf("\n%s", md)
	if path := os.Getenv("JOURNEY_REPORT"); path != "" {
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			t.Errorf("write %s: %v", path, err)
		}
	}
}
