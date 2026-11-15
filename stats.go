package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Histogram records latencies in 0.1 ms buckets, up to the request timeout.
// Safe to use from many goroutines.
type Histogram struct {
	buckets [histBuckets]atomic.Int64
	count   atomic.Int64
}

const (
	histBucketWidth = 100 * time.Microsecond
	histBuckets     = int(RequestTimeout/histBucketWidth) + 1
)

func (h *Histogram) Record(latency time.Duration) {
	h.buckets[min(int(latency/histBucketWidth), histBuckets-1)].Add(1)
	h.count.Add(1)
}

// Percentile returns the latency that p percent of the requests stayed under.
func (h *Histogram) Percentile(p float64) time.Duration {
	target := int64(float64(h.count.Load()) * p / 100)
	var seen int64
	for i := range h.buckets {
		seen += h.buckets[i].Load()
		if seen > target {
			return time.Duration(i+1) * histBucketWidth
		}
	}
	return 0
}

// Stats counts data requests across all devices. Safe to use from many goroutines.
type Stats struct {
	// Every request, for the once-per-second status line.
	sent         atomic.Int64
	failed       atomic.Int64
	totalLatency atomic.Int64 // nanoseconds, successful requests only

	// Only requests made while measuring is on: these decide pass or fail.
	measuring      atomic.Bool
	measuredSent   atomic.Int64
	measuredFailed atomic.Int64
	latency        Histogram
}

func (s *Stats) Record(ok bool, latency time.Duration) {
	measuring := s.measuring.Load()
	if !ok {
		s.failed.Add(1)
		if measuring {
			s.measuredFailed.Add(1)
		}
		return
	}
	s.sent.Add(1)
	s.totalLatency.Add(int64(latency))
	if measuring {
		s.measuredSent.Add(1)
		s.latency.Record(latency)
	}
}

// PrintEverySecond prints one status line per second until ctx is cancelled.
func (s *Stats) PrintEverySecond(ctx context.Context, devices int) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastSent, lastFailed, lastLatency int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sent, failed, latency := s.sent.Load(), s.failed.Load(), s.totalLatency.Load()

		sentNow := sent - lastSent
		avg := time.Duration(0)
		if sentNow > 0 {
			avg = time.Duration((latency - lastLatency) / sentNow)
		}
		fmt.Printf("devices=%d  sent/s=%d  failed/s=%d  avg_latency=%s\n",
			devices, sentNow, failed-lastFailed, avg.Round(10*time.Microsecond))

		lastSent, lastFailed, lastLatency = sent, failed, latency
	}
}

// QueryStats counts the read requests of the simulated dashboard user.
type QueryStats struct {
	ok      atomic.Int64
	failed  atomic.Int64
	stale   atomic.Int64 // answered, but an accepted reading was still missing after FreshnessLimit
	latency Histogram
}
