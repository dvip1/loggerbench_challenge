package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// Audit remembers every reading one device sent, so it can be compared with
// what the server gives back. Only a few random devices are audited; the
// server cannot know which ones.
type Audit struct {
	mu    sync.Mutex
	sent  map[int64][]float32 // timestamp -> sensor values
	acked map[int64]bool      // timestamps the server answered 2xx for
}

func newAudit() *Audit {
	return &Audit{sent: map[int64][]float32{}, acked: map[int64]bool{}}
}

func (a *Audit) recordSent(ts int64, sensors []float32) {
	a.mu.Lock()
	a.sent[ts] = sensors
	a.mu.Unlock()
}

func (a *Audit) recordAcked(ts int64) {
	a.mu.Lock()
	a.acked[ts] = true
	a.mu.Unlock()
}

// AuditResult is the outcome of comparing stored readings with sent readings.
type AuditResult struct {
	Devices   int
	Readings  int // accepted readings that were looked for
	Missing   int // accepted (2xx) but not returned
	Corrupt   int // returned, but never sent or with different values
	Duplicate int // returned twice, or not sorted by timestamp
	Errors    int // history request failed
}

func (r AuditResult) OK() bool {
	return r.Devices > 0 && r.Missing+r.Corrupt+r.Duplicate+r.Errors == 0
}

func (r AuditResult) String() string {
	return fmt.Sprintf("devices=%d readings=%d missing=%d corrupt=%d duplicate=%d errors=%d",
		r.Devices, r.Readings, r.Missing, r.Corrupt, r.Duplicate, r.Errors)
}

// verifyAudits reads back the history of every audited device and checks that
// each accepted reading with from <= timestamp <= to is there, unchanged, once.
func verifyAudits(client *http.Client, baseURL string, devices []*Device, from, to int64) AuditResult {
	var res AuditResult
	problems := 0
	problem := func(format string, args ...any) {
		if problems++; problems <= 5 {
			fmt.Printf("  audit: "+format+"\n", args...)
		}
	}

	for _, d := range devices {
		if d.audit == nil {
			continue
		}
		res.Devices++
		rows, err := d.Owner.AllReadings(client, baseURL, d.Srno, from, to)
		if err != nil {
			res.Errors++
			problem("%s: reading history failed: %v", d.Srno, err)
			continue
		}

		d.audit.mu.Lock()
		seen := map[int64]bool{}
		prev := int64(-1 << 62)
		for _, row := range rows {
			if row.Timestamp <= prev {
				res.Duplicate++
				problem("%s: timestamp %d is repeated or out of order", d.Srno, row.Timestamp)
			}
			prev = row.Timestamp
			if sensors, ok := d.audit.sent[row.Timestamp]; !ok || !row.matches(row.Timestamp, sensors) {
				res.Corrupt++
				problem("%s: reading at %d is not what the device sent", d.Srno, row.Timestamp)
			}
			seen[row.Timestamp] = true
		}
		for ts := range d.audit.acked {
			if ts < from || ts > to {
				continue
			}
			res.Readings++
			if !seen[ts] {
				res.Missing++
				problem("%s: reading at %d was accepted but is not in the history", d.Srno, ts)
			}
		}
		d.audit.mu.Unlock()
	}
	return res
}

// runViewer plays a dashboard user while the devices are sending: every
// ViewerInterval it opens a random device, asking for its latest reading and
// its last minute of history. Only counted while stats.measuring is on.
func runViewer(ctx context.Context, client *http.Client, baseURL string, devices []*Device, stats *Stats, q *QueryStats) {
	ticker := time.NewTicker(ViewerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !stats.measuring.Load() {
			continue
		}
		d := devices[rand.IntN(len(devices))]
		acked := d.lastAcked.Load() // accepted before we ask, so it may be at most FreshnessLimit old
		now := time.Now().Unix()

		start := time.Now()
		latest, err := d.Owner.Latest(client, baseURL, d.Srno)
		if err == nil {
			_, err = d.Owner.Readings(client, baseURL, d.Srno, now-60, now, HistoryPageLimit)
		}
		elapsed := time.Since(start) / 2 // two requests

		switch {
		case err != nil && acked == 0:
			// Nothing accepted from this device yet; a 404 is fine.
		case err != nil:
			q.failed.Add(1)
		case acked > 0 && latest.Timestamp < acked-int64(FreshnessLimit/time.Second):
			q.stale.Add(1)
		default:
			q.ok.Add(1)
			q.latency.Record(elapsed)
		}
	}
}
