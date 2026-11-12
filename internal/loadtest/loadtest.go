package loadtest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	Router, Model, DirectModel                   string
	Direct                                       []string
	Weights                                      []float64
	APIKey, DirectKey, MetricsToken, DatabaseURL string
	Concurrency, Requests, Runs, Tokens          int
	AbortFraction                                float64
	AbortAfter                                   int
	Timeout, Settle                              time.Duration
	Mock                                         bool
	MockDelay                                    string
	CheckPerformance                             bool
	MaxAddedP95MS, MaxTPSLoss                    float64
}

func (o Options) Validate() error {
	if o.Concurrency < 1 || o.Concurrency > 16384 || o.Requests < o.Concurrency || o.Requests > 1000000 || o.Runs < 1 || o.Runs > 100 || o.Tokens < 2 || o.Tokens > 1000000 {
		return errors.New("require concurrency 1..16384, requests concurrency..1000000, runs 1..100 and tokens 2..1000000")
	}
	if o.Timeout <= 0 || o.Settle < 0 || math.IsNaN(o.AbortFraction) || o.AbortFraction < 0 || o.AbortFraction >= 1 || o.AbortAfter < 1 || o.AbortAfter >= o.Tokens {
		return errors.New("require positive timeout, nonnegative settle, abort fraction in [0,1), and abort-after below tokens")
	}
	if o.MaxAddedP95MS < 0 || math.IsNaN(o.MaxAddedP95MS) || math.IsInf(o.MaxAddedP95MS, 0) || o.MaxTPSLoss < 0 || o.MaxTPSLoss >= 1 || math.IsNaN(o.MaxTPSLoss) {
		return errors.New("invalid performance thresholds")
	}
	if len(o.Direct) == 0 || len(o.Direct) != len(o.Weights) {
		return errors.New("provide one positive weight for each direct endpoint")
	}
	for _, endpoint := range append([]string{o.Router}, o.Direct...) {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
			return errors.New("endpoints must be distinct HTTP(S) origins without credentials, paths, queries or fragments")
		}
	}
	for i, endpoint := range o.Direct {
		duplicate := false
		for _, previous := range o.Direct[:i] {
			duplicate = duplicate || strings.TrimRight(previous, "/") == strings.TrimRight(endpoint, "/")
		}
		if strings.TrimRight(endpoint, "/") == strings.TrimRight(o.Router, "/") || !isPositive(o.Weights[i]) || duplicate {
			return errors.New("direct endpoints must differ from the router and each other; weights must be finite and positive")
		}
	}
	if o.MockDelay != "" {
		delay, err := time.ParseDuration(o.MockDelay)
		if err != nil || delay < 0 || !o.Mock {
			return errors.New("mock-delay requires mock mode and a nonnegative duration")
		}
	}
	if o.APIKey == "" || o.Model == "" || o.MetricsToken == "" {
		return errors.New("model, OLR_API_KEY and OLR_METRICS_TOKEN are required")
	}
	return nil
}

func isPositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

type Summary struct {
	Requests, Completed, Aborted, PeakClients              int
	Errors                                                 map[string]int `json:"errors"`
	P50TTFTMS, P95TTFTMS, MedianTPS, AggregateTPS, Seconds float64
}

type Comparison struct {
	Run                               int
	Protocol                          string
	Direct, Router                    Summary
	AddedP95TTFTMS                    float64
	MedianTPSRatio, AggregateTPSRatio float64
	PerformancePassed                 bool
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type Cancellation struct {
	Run      int
	Protocol string
	Summary
}

type PeakRuntime struct {
	Samples                                                                       int
	CPUCores, ResidentBytes, HeapInUse, Goroutines, ReplicaInFlight, UserInFlight float64
}

type Report struct {
	Concurrency, RequestsPerBatch, Runs, Tokens int
	Mock                                        bool
	MockDelay                                   string
	Comparisons                                 []Comparison
	Cancellations                               []Cancellation
	Runtime                                     []RuntimeSnapshot
	Peak                                        PeakRuntime
	Checks                                      []Check
	RouterRequests, UsageRows                   int64
	ReplicaRequests                             map[string]int
	ReplicaAborted                              map[string]int
	AccountingVerified                          bool
	LeakStabilityVerified                       bool
	PerformanceEnforced                         bool
	MaxAddedP95MS, MaxTPSLoss                   float64
	Passed                                      bool
	GoVersion, Platform                         string
	AvailableCPUs, ClientGOMAXPROCS             int
	Limitations                                 []string
}

func (r *Report) check(name string, passed bool, detail string) {
	r.Checks = append(r.Checks, Check{name, passed, detail})
	r.Passed = r.Passed && passed
}

type runner struct {
	o       Options
	load    *http.Client
	probes  *http.Client
	observe sync.Mutex
	chooser weightedTargets
	report  Report
	peak    PeakRuntime
	account *accounting
}

// Run never writes credentials, response content, user IDs or database URLs to
// its report. The database connection, when configured, is read-only.
func Run(ctx context.Context, options Options) (result Report, err error) {
	defer func() {
		if err != nil {
			result.Passed = false
		}
	}()
	if err := options.Validate(); err != nil {
		return Report{}, err
	}
	o := options
	o.Direct = slices.Clone(options.Direct)
	o.Router = strings.TrimRight(o.Router, "/")
	for i := range o.Direct {
		o.Direct[i] = strings.TrimRight(o.Direct[i], "/")
	}
	if o.DirectModel == "" {
		o.DirectModel = o.Model
	}
	r := &runner{o: o, load: newClient(o.Concurrency, o.Timeout), probes: newClient(4, 5*time.Second), chooser: weightedTargets{weights: o.Weights, pending: make([]int, len(o.Direct))}}
	defer r.load.CloseIdleConnections()
	defer r.probes.CloseIdleConnections()
	r.report = Report{Concurrency: o.Concurrency, RequestsPerBatch: o.Requests, Runs: o.Runs, Tokens: o.Tokens, Mock: o.Mock, MockDelay: o.MockDelay, Passed: true, PerformanceEnforced: o.CheckPerformance, MaxAddedP95MS: o.MaxAddedP95MS, MaxTPSLoss: o.MaxTPSLoss, ReplicaRequests: map[string]int{}, ReplicaAborted: map[string]int{}}
	r.report.GoVersion, r.report.Platform = runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH
	r.report.AvailableCPUs, r.report.ClientGOMAXPROCS = runtime.NumCPU(), runtime.GOMAXPROCS(0)
	if o.DatabaseURL == "" {
		r.report.Limitations = append(r.report.Limitations, "Database accounting was not verified; set OLR_LOADTEST_DATABASE_URL.")
	}
	if o.Runs < 3 {
		r.report.Limitations = append(r.report.Limitations, "Fewer than three runs cannot establish memory stability.")
	}
	if !o.CheckPerformance {
		r.report.Limitations = append(r.report.Limitations, "Performance thresholds were measured but not enforced.")
	}
	if !o.Mock {
		r.report.Limitations = append(r.report.Limitations, "Backend cancellation counters are unavailable; router lease release is checked.")
	}
	if o.AbortFraction == 0 {
		r.report.Limitations = append(r.report.Limitations, "Intentional disconnect checks were disabled.")
	}
	if err := r.openAccounting(ctx); err != nil {
		return r.report, err
	}
	if r.account != nil {
		defer r.account.pool.Close()
	}
	if _, err := r.snapshot(ctx); err != nil {
		return r.report, fmt.Errorf("preflight: %w", err)
	}
	initialMocks, err := r.mockStats(ctx)
	if err != nil {
		return r.report, err
	}
	// Prime caches and runtime pools at full concurrency, then establish an idle
	// baseline after closing keep-alives. Each measured path is warmed again.
	for _, protocol := range []string{"openai", "anthropic"} {
		if !r.warm(ctx, protocol) {
			return r.report, errors.New("warm-up failed; inspect checks")
		}
	}
	initial, err := r.idle(ctx)
	if err != nil {
		return r.report, err
	}
	r.report.Runtime = append(r.report.Runtime, initial)
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	var monitorWG sync.WaitGroup
	monitorWG.Go(func() { r.monitor(monitorCtx) })
	defer func() { stopMonitor(); monitorWG.Wait() }()
	for run := 1; run <= o.Runs; run++ {
		for _, protocol := range []string{"openai", "anthropic"} {
			if !r.warm(ctx, protocol) {
				return r.report, errors.New("warm-up failed; inspect checks")
			}
			var direct, router Summary
			if run%2 == 1 {
				direct = r.batch(ctx, protocol, false, o.Requests, 0)
				router = r.batch(ctx, protocol, true, o.Requests, 0)
			} else {
				router = r.batch(ctx, protocol, true, o.Requests, 0)
				direct = r.batch(ctx, protocol, false, o.Requests, 0)
			}
			c := Comparison{Run: run, Protocol: protocol, Direct: direct, Router: router, AddedP95TTFTMS: router.P95TTFTMS - direct.P95TTFTMS}
			if direct.MedianTPS > 0 && direct.AggregateTPS > 0 {
				c.MedianTPSRatio, c.AggregateTPSRatio = router.MedianTPS/direct.MedianTPS, router.AggregateTPS/direct.AggregateTPS
			}
			c.PerformancePassed = c.AddedP95TTFTMS < o.MaxAddedP95MS && c.MedianTPSRatio >= 1-o.MaxTPSLoss && c.AggregateTPSRatio >= 1-o.MaxTPSLoss
			r.report.Comparisons = append(r.report.Comparisons, c)
			r.report.check(fmt.Sprintf("%s run %d: complete streams", protocol, run), direct.Completed == o.Requests && router.Completed == o.Requests, "direct and router must have zero errors")
			r.report.check(fmt.Sprintf("%s run %d: full client concurrency", protocol, run), direct.PeakClients == o.Concurrency && router.PeakClients == o.Concurrency, "requested concurrent clients must actually overlap")
			if o.CheckPerformance {
				r.report.check(fmt.Sprintf("%s run %d: performance", protocol, run), c.PerformancePassed, "p95 TTFT difference and both per-stream/aggregate throughput ratios")
			}
			aborts := int(float64(o.Concurrency) * o.AbortFraction)
			if aborts > 0 {
				cancelled := r.batch(ctx, protocol, true, o.Concurrency, aborts)
				r.report.Cancellations = append(r.report.Cancellations, Cancellation{run, protocol, cancelled})
				r.report.check(fmt.Sprintf("%s run %d: intentional disconnects", protocol, run), cancelled.Aborted == aborts && cancelled.Completed+cancelled.Aborted == o.Concurrency, "aborted streams are separate from errors and performance samples")
			}
		}
		snapshot, err := r.idle(ctx)
		if err != nil {
			return r.report, err
		}
		r.report.Runtime = append(r.report.Runtime, snapshot)
	}
	stopMonitor()
	monitorWG.Wait()
	r.report.Peak = r.peak
	r.report.check("runtime samples collected", r.peak.Samples > 0, "protected Prometheus snapshots every 250 ms")
	last := r.report.Runtime[len(r.report.Runtime)-1]
	r.report.check("replica leases and user concurrency slots released", last.ReplicaInFlight == 0 && last.UserInFlight == 0, "both protected metrics and /v1/me/usage")
	r.report.check("no usage writer failures or drops", last.WriteFailures == initial.WriteFailures && last.Dropped == initial.Dropped, "compare counters before/after the run")
	minG, maxG, minHeap, maxHeap, minRSS, maxRSS := math.Inf(1), 0.0, math.Inf(1), 0.0, math.Inf(1), 0.0
	for _, snapshot := range r.report.Runtime[1:] {
		minG, maxG = min(minG, snapshot.IdleGoroutinesMin), max(maxG, snapshot.IdleGoroutinesMin)
		minHeap, maxHeap = min(minHeap, snapshot.HeapInUse), max(maxHeap, snapshot.HeapInUse)
		minRSS, maxRSS = min(minRSS, snapshot.ResidentBytes), max(maxRSS, snapshot.ResidentBytes)
	}
	if o.Runs >= 3 {
		goroutinesStable := maxG-minG <= 4
		heapStable := maxHeap-minHeap <= max(32<<20, minHeap*.25)
		rssStable := maxRSS-minRSS <= max(64<<20, minRSS*.25)
		r.report.check("post-run idle goroutines stabilize", goroutinesStable, "quiet-window minima: post-run max-min <= four health-check/scrape goroutines; initial pool warming is separate")
		r.report.check("idle heap stable across runs", heapStable, "max-min <= max(32 MiB, 25% of minimum); see raw snapshots")
		r.report.check("idle RSS stable across runs", rssStable, "max-min <= max(64 MiB, 25% of minimum); see raw snapshots")
		r.report.LeakStabilityVerified = goroutinesStable && heapStable && rssStable
	}
	if r.account != nil {
		r.report.AccountingVerified = r.report.UsageRows == r.report.RouterRequests
		r.report.check("one usage row per router request", r.report.AccountingVerified, fmt.Sprintf("expected %d, observed %d, including warm-ups and disconnects", r.report.RouterRequests, r.report.UsageRows))
	}
	for replica, count := range last.RequestsByReplica {
		r.report.ReplicaRequests[replica] = int(count - initial.RequestsByReplica[replica])
	}
	if o.Mock {
		finalMocks, err := r.mockStats(ctx)
		if err != nil {
			return r.report, err
		}
		cancelled := 0
		for i, stats := range finalMocks {
			name := fmt.Sprintf("replica_%d", i+1)
			r.report.ReplicaAborted[name] = stats.Aborted - initialMocks[i].Aborted
			cancelled += stats.Aborted - initialMocks[i].Aborted
			r.report.check(name+" has no in-flight requests", stats.InFlight == 0, "mock counter")
		}
		want := 0
		for _, cancellation := range r.report.Cancellations {
			want += cancellation.Aborted
		}
		if want > 0 {
			r.report.check("disconnects reach the backends", cancelled >= want, fmt.Sprintf("at least %d expected; %d observed", want, cancelled))
		}
	}
	if ctx.Err() != nil {
		return r.report, ctx.Err()
	}
	return r.report, nil
}

func (r *runner) warm(ctx context.Context, protocol string) bool {
	for _, router := range []bool{false, true} {
		summary := r.batch(ctx, protocol, router, r.o.Concurrency, 0)
		if summary.Completed != r.o.Concurrency {
			r.report.check("warm-up", false, fmt.Sprintf("router=%t protocol=%s errors=%v", router, protocol, summary.Errors))
			return false
		}
	}
	return true
}

type weightedTargets struct {
	mu      sync.Mutex
	weights []float64
	pending []int
}

func (w *weightedTargets) acquire() (int, func()) {
	w.mu.Lock()
	best := 0
	for i := 1; i < len(w.weights); i++ {
		if float64(w.pending[i])/w.weights[i] < float64(w.pending[best])/w.weights[best] {
			best = i
		}
	}
	w.pending[best]++
	w.mu.Unlock()
	return best, func() { w.mu.Lock(); w.pending[best]--; w.mu.Unlock() }
}

func (r *runner) batch(ctx context.Context, protocol string, router bool, count, aborts int) Summary {
	results := make([]sample, count)
	var next, active, peak atomic.Int64
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for range r.o.Concurrency {
		ready.Add(1)
		done.Go(func() {
			ready.Done()
			<-start
			for {
				i := int(next.Add(1) - 1)
				if i >= count {
					return
				}
				a := active.Add(1)
				for p := peak.Load(); a > p && !peak.CompareAndSwap(p, a); p = peak.Load() {
				}
				endpoint, model, key := r.o.Router, r.o.Model, r.o.APIKey
				release := func() {}
				if !router {
					var index int
					index, release = r.chooser.acquire()
					endpoint, model, key = r.o.Direct[index], r.o.DirectModel, r.o.DirectKey
				}
				results[i] = request(ctx, r.load, r.o, endpoint, model, key, protocol, i < aborts)
				release()
				active.Add(-1)
			}
		})
	}
	ready.Wait()
	started := time.Now()
	close(start)
	done.Wait()
	summary := summarize(results, time.Since(started))
	summary.PeakClients = int(peak.Load())
	if router {
		r.report.RouterRequests += int64(count)
	}
	return summary
}

func summarize(samples []sample, elapsed time.Duration) Summary {
	s := Summary{Requests: len(samples), Seconds: elapsed.Seconds(), Errors: map[string]int{}}
	var ttft, tps []float64
	var tokens int64
	for _, sample := range samples {
		switch {
		case sample.err != "":
			s.Errors[sample.err]++
		case sample.aborted:
			s.Aborted++
		default:
			s.Completed++
			ttft, tps = append(ttft, sample.ttft), append(tps, sample.tps)
			tokens += sample.tokens
		}
	}
	s.P50TTFTMS, s.P95TTFTMS, s.MedianTPS = quantile(ttft, .5), quantile(ttft, .95), quantile(tps, .5)
	if elapsed > 0 {
		s.AggregateTPS = float64(tokens) / elapsed.Seconds()
	}
	return s
}

func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	slices.Sort(values)
	return values[max(0, int(math.Ceil(float64(len(values))*q))-1)]
}
