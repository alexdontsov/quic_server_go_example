package gapstats_test

import (
	"testing"
	"time"

	"quic_server_go/internal/gapstats"
)

func TestSummary(t *testing.T) {
	t.Parallel()

	var r gapstats.Recorder
	// Интервалы: 10, 10, 10, 10, 50 мс.
	for _, ms := range []int{0, 10, 20, 30, 40, 90} {
		r.Record(time.Duration(ms) * time.Millisecond)
	}

	s := r.Summary()
	if s.Count != 6 {
		t.Fatalf("count: got %d, want 6", s.Count)
	}
	if s.P50 != 10*time.Millisecond {
		t.Fatalf("p50: got %v, want 10ms", s.P50)
	}
	if s.Max != 50*time.Millisecond {
		t.Fatalf("max: got %v, want 50ms", s.Max)
	}
	if s.P95 != 50*time.Millisecond {
		t.Fatalf("p95: got %v, want 50ms", s.P95)
	}
}

func TestSummaryTooFewSamples(t *testing.T) {
	t.Parallel()

	var r gapstats.Recorder
	r.Record(time.Millisecond)
	s := r.Summary()
	if s.Count != 1 || s.Max != 0 {
		t.Fatalf("unexpected summary for a single sample: %+v", s)
	}
}
