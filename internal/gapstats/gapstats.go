// Package gapstats собирает моменты прихода сообщений и считает статистику
// интервалов между ними. Используется демо, сравнивающими транспорты.
package gapstats

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// Recorder накапливает временные метки прихода сообщений.
type Recorder struct {
	mu    sync.Mutex
	times []time.Duration
}

// Record фиксирует момент прихода очередного сообщения (время от старта).
func (r *Recorder) Record(t time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.times = append(r.times, t)
}

// Count возвращает число зафиксированных сообщений.
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.times)
}

// Summary — перцентили интервалов между соседними сообщениями.
type Summary struct {
	Count         int
	P50, P95, Max time.Duration
}

// Summary считает статистику по накопленным меткам.
func (r *Recorder) Summary() Summary {
	r.mu.Lock()
	times := slices.Clone(r.times)
	r.mu.Unlock()

	s := Summary{Count: len(times)}
	if len(times) < 2 {
		return s
	}
	slices.Sort(times)
	gaps := make([]time.Duration, 0, len(times)-1)
	for i := 1; i < len(times); i++ {
		gaps = append(gaps, times[i]-times[i-1])
	}
	slices.Sort(gaps)
	s.P50 = percentile(gaps, 50)
	s.P95 = percentile(gaps, 95)
	s.Max = gaps[len(gaps)-1]
	return s
}

// String форматирует сводку в одну строку с миллисекундами.
func (s Summary) String() string {
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	return fmt.Sprintf("gap p50 / p95 / max: %.1f / %.1f / %.1f ms", ms(s.P50), ms(s.P95), ms(s.Max))
}

// percentile берёт ближайший ранг по отсортированному срезу (nearest-rank).
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100 // округление вверх
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
