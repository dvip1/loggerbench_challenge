package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	numDevices := flag.Int("devices", 1, "number of devices to simulate (0 = only run the conformance checks)")
	baseURL := flag.String("url", BaseURL, "server base URL")
	maxConns := flag.Int("max-conns", 1000, "max open connections to the server")
	duration := flag.Duration("duration", 0, "how long to send data, e.g. 5m (0 = until Ctrl+C)")
	crashTest := flag.Bool("crash-test", false, "after the load, ask you to kill -9 and restart the server, then check that nothing accepted was lost")
	tokenList := flag.String("tokens", "", "load only: use these registration token(s) from your dashboard, comma-separated, instead of letting the benchmark create its own users (nothing is read back or checked)")
	flag.Parse()

	// Ctrl+C ends the load early; the results are still checked and printed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// One shared client with keep-alive, so connections are reused.
	client := &http.Client{
		Timeout: RequestTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        *maxConns,
			MaxIdleConnsPerHost: *maxConns,
			MaxConnsPerHost:     *maxConns,
		},
	}

	// 1. Health check.
	if err := checkHealth(client, *baseURL); err != nil {
		fmt.Println("server is not healthy:", err)
		os.Exit(1)
	}

	// 2. Tokens: either the ones given with -tokens, or the benchmark signs up
	// its own users and creates tokens through the API (after the conformance checks).
	result := &Result{}
	var tokens []*RegToken
	for _, t := range parseTokens(*tokenList) {
		tokens = append(tokens, &RegToken{Token: t})
	}
	benchmark := len(tokens) == 0 // full benchmark: checks + load + reading the data back
	if benchmark {
		passed, total := runChecks(client, *baseURL)
		result.add("conformance", passed == total, fmt.Sprintf("%d/%d checks", passed, total))
		if *numDevices == 0 {
			os.Exit(result.print(0))
		}
		var err error
		if tokens, err = createBenchmarkTokens(client, *baseURL); err != nil {
			fmt.Println("cannot create users and tokens for the load:", err)
			os.Exit(1)
		}
	}

	// 3. Create and register devices, spreading them evenly over the tokens.
	devices := make([]*Device, *numDevices)
	for i := range devices {
		t := tokens[i%len(tokens)]
		devices[i] = NewDevice(i, t.Token)
		devices[i].Owner = t.Owner
	}
	registered := registerAll(client, *baseURL, devices)
	fmt.Printf("registered %d/%d devices\n", registered, len(devices))
	result.add("registration", registered == len(devices), fmt.Sprintf("%d/%d devices", registered, len(devices)))
	if registered == 0 {
		os.Exit(1)
	}
	var live []*Device // devices that have a key
	for _, d := range devices {
		if d.aead != nil {
			live = append(live, d)
		}
	}
	if benchmark {
		for _, i := range rand.Perm(len(live))[:min(AuditDevices, len(live))] {
			live[i].audit = newAudit()
		}
	}

	// 4. Send data, and play a dashboard user who looks at devices meanwhile.
	stats, queries := &Stats{}, &QueryStats{}
	runCtx, stopRun := context.WithCancel(context.Background())
	go stats.PrintEverySecond(runCtx, len(live))

	var wg sync.WaitGroup
	for _, d := range live {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Run(runCtx, client, *baseURL, stats)
		}()
	}
	if benchmark {
		go runViewer(runCtx, client, *baseURL, live, stats, queries)
	}

	started := time.Now()
	stats.measuring.Store(true)
	if *duration > 0 {
		select {
		case <-time.After(*duration):
		case <-ctx.Done():
		}
	} else {
		<-ctx.Done()
	}
	stats.measuring.Store(false)
	measured := time.Since(started)
	interrupted := ctx.Err() != nil
	stop() // a second Ctrl+C now quits immediately

	// 5. Crash test: the devices keep sending while the server is killed and restarted.
	if benchmark && *crashTest && !interrupted {
		fmt.Println("checking stored readings before the crash...")
		before := verifyAudits(client, *baseURL, live, started.Unix()-1, time.Now().Add(-FreshnessLimit).Unix())
		result.add("audit before crash", before.OK(), before.String())
		ok, detail := runCrashTest(*baseURL, stats, len(live))
		result.add("crash recovery", ok, detail)
	}
	stopRun()
	wg.Wait()

	// 6. Results.
	sent, failed := stats.measuredSent.Load(), stats.measuredFailed.Load()
	rate := min(1, float64(sent+failed)/(float64(len(live))*measured.Seconds()))
	p99 := stats.latency.Percentile(99)
	result.add("ingest", failed == 0 && sent > 0 && rate >= MinSendRate && p99 <= MaxIngestP99,
		fmt.Sprintf("sent=%d failed=%d rate=%.1f%% p50=%s p99=%s", sent, failed, rate*100, stats.latency.Percentile(50), p99))

	if benchmark {
		qp99 := queries.latency.Percentile(99)
		result.add("queries under load", queries.failed.Load() == 0 && queries.stale.Load() == 0 && queries.ok.Load() > 0 && qp99 <= MaxQueryP99,
			fmt.Sprintf("ok=%d failed=%d stale=%d p50=%s p99=%s", queries.ok.Load(), queries.failed.Load(), queries.stale.Load(), queries.latency.Percentile(50), qp99))

		fmt.Println("checking stored readings...")
		time.Sleep(FreshnessLimit)
		audit := verifyAudits(client, *baseURL, live, started.Unix()-1, time.Now().Unix()+1)
		result.add("audit", audit.OK(), audit.String())
	}
	// Everything the server said 2xx to, checks excluded: divide the data directory size by this.
	result.lines = append(result.lines, fmt.Sprintf("%-20s %d", "accepted readings", stats.sent.Load()))
	os.Exit(result.print(len(live)))
}

// Result collects one pass/fail line per part of the run.
type Result struct {
	lines  []string
	failed bool
}

func (r *Result) add(name string, ok bool, detail string) {
	verdict := "PASS"
	if !ok {
		verdict = "FAIL"
		r.failed = true
	}
	r.lines = append(r.lines, fmt.Sprintf("%-20s %s  %s", name, verdict, detail))
}

// print shows the summary and returns the process exit code.
func (r *Result) print(devices int) int {
	fmt.Println("\n==== RESULT ====")
	for _, line := range r.lines {
		fmt.Println(line)
	}
	if r.failed {
		fmt.Println("VERDICT: FAIL")
		return 1
	}
	if devices > 0 {
		fmt.Printf("VERDICT: PASS at %d devices\n", devices)
	} else {
		fmt.Println("VERDICT: PASS")
	}
	return 0
}

// createBenchmarkTokens signs up two users with two tokens (groups) each.
func createBenchmarkTokens(client *http.Client, baseURL string) ([]*RegToken, error) {
	var tokens []*RegToken
	for u := range 2 {
		user, err := newUser(client, baseURL)
		if err != nil {
			return nil, err
		}
		for g := range 2 {
			t, err := user.CreateToken(client, baseURL, fmt.Sprintf("bench-user%d-group%d", u+1, g+1))
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, t)
		}
	}
	return tokens, nil
}

// runCrashTest waits for the server to be killed and restarted by hand while
// the devices keep sending with the keys they already have. Once the server is
// back, no request may fail any more.
func runCrashTest(baseURL string, stats *Stats, devices int) (bool, string) {
	probe := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	healthy := func() bool {
		resp, err := probe.Get(baseURL + HealthPath)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	waitFor := func(want bool, timeout time.Duration) bool {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if healthy() == want {
				return true
			}
			time.Sleep(250 * time.Millisecond)
		}
		return false
	}

	fmt.Println("\n>>> CRASH TEST: kill -9 every process of your server now, then start it again. Devices keep sending. <<<")
	if !waitFor(false, 5*time.Minute) {
		return false, "the server never went down"
	}
	fmt.Println(">>> server is down, waiting for it to come back <<<")
	if !waitFor(true, 2*time.Minute) {
		return false, "the server did not come back within 2 minutes"
	}
	fmt.Println(">>> server is back, devices must work again with their old keys <<<")
	time.Sleep(RecoveryGrace)
	sent, failed := stats.sent.Load(), stats.failed.Load()
	time.Sleep(RecoveryWindow)
	sent, failed = stats.sent.Load()-sent, stats.failed.Load()-failed

	want := int64(float64(devices) * RecoveryWindow.Seconds() * MinSendRate)
	detail := fmt.Sprintf("after restart: sent=%d failed=%d in %s", sent, failed, RecoveryWindow)
	return failed == 0 && sent >= want, detail
}

// parseTokens splits "a, b,,c" into ["a", "b", "c"].
func parseTokens(list string) []string {
	var tokens []string
	for _, t := range strings.Split(list, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

func checkHealth(client *http.Client, baseURL string) error {
	resp, err := client.Get(baseURL + HealthPath)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	fmt.Printf("health: %s\n", body)
	return nil
}

// registerAll registers devices, RegisterConcurrency at a time, and returns how many succeeded.
func registerAll(client *http.Client, baseURL string, devices []*Device) int {
	var ok atomic.Int64
	var wg sync.WaitGroup
	slots := make(chan struct{}, RegisterConcurrency)

	for _, d := range devices {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := d.Register(client, baseURL); err != nil {
				fmt.Println(err)
				return
			}
			ok.Add(1)
		}()
	}
	wg.Wait()
	return int(ok.Load())
}
