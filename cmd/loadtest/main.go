// Command loadtest is a streaming load generator for the Studio /listen route.
//
// It opens many concurrent HTTP connections to an audio-stream endpoint, drains
// each response body (by default paced at real time, like a real player), ramps
// the connection count on a schedule, and reports how many listeners stay
// healthy versus how many the server drops or refuses.
//
// It is intentionally dependency-free (standard library only) so it can be run
// straight from a repo checkout with `go run ./cmd/loadtest`.
//
// Example:
//
//	go run ./cmd/loadtest \
//	  -url http://127.0.0.1:7080/studios/reformation-rw/listen \
//	  -start 200 -step 200 -step-interval 45s -max 20000 -hold 60s -out ramp.csv
package main

import (
	"context"
	"crypto/tls"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type config struct {
	url          string
	start        int
	step         int
	stepInterval time.Duration
	max          int
	hold         time.Duration
	rate         int
	realtime     bool
	spawnDelay   time.Duration
	dialTimeout  time.Duration
	reportEvery  time.Duration
	out          string
	insecure     bool
}

func parseFlags() config {
	var cfg config
	var drain string
	flag.StringVar(&cfg.url, "url", "", "target /listen URL (required)")
	flag.IntVar(&cfg.start, "start", 100, "listeners to open immediately")
	flag.IntVar(&cfg.step, "step", 100, "listeners to add each step-interval (0 disables ramp)")
	flag.DurationVar(&cfg.stepInterval, "step-interval", 30*time.Second, "time between ramp steps")
	flag.IntVar(&cfg.max, "max", 5000, "maximum concurrent listeners")
	flag.DurationVar(&cfg.hold, "hold", 60*time.Second, "time to hold at max before stopping")
	flag.IntVar(&cfg.rate, "rate", 16000, "per-connection drain rate in bytes/sec (128 kbps = 16000)")
	flag.StringVar(&drain, "drain", "realtime", "body drain mode: realtime (pace to -rate) or fast (read as fast as possible)")
	flag.DurationVar(&cfg.spawnDelay, "spawn-delay", 2*time.Millisecond, "delay between individual dials while spawning")
	flag.DurationVar(&cfg.dialTimeout, "dial-timeout", 10*time.Second, "TCP dial + response-header timeout")
	flag.DurationVar(&cfg.reportEvery, "report-every", 5*time.Second, "progress report / CSV row interval")
	flag.StringVar(&cfg.out, "out", "", "optional CSV output file for progress rows")
	flag.BoolVar(&cfg.insecure, "insecure", false, "skip TLS certificate verification")
	flag.Parse()

	if cfg.url == "" {
		fmt.Fprintln(os.Stderr, "loadtest: -url is required")
		flag.Usage()
		os.Exit(2)
	}
	switch drain {
	case "realtime":
		cfg.realtime = true
	case "fast":
		cfg.realtime = false
	default:
		fmt.Fprintf(os.Stderr, "loadtest: invalid -drain %q (want realtime|fast)\n", drain)
		os.Exit(2)
	}
	if cfg.max < cfg.start {
		cfg.max = cfg.start
	}
	return cfg
}

// connStat is the live per-connection accounting a worker publishes so the
// reporter can compute per-listener throughput without locking the worker.
type connStat struct {
	bytes   atomic.Int64
	firstNS atomic.Int64 // unix-nano of first body byte, 0 until received
	alive   atomic.Bool
}

type stats struct {
	intended     atomic.Int64
	connected    atomic.Int64 // currently streaming
	connectErrs  atomic.Int64
	earlyClosed  atomic.Int64
	closeEnded   atomic.Int64 // clean EOF mid-test => server ended the stream (eviction / shutdown)
	closeReset   atomic.Int64 // connection reset / unexpected EOF
	closeOther   atomic.Int64
	totalBytes   atomic.Int64
	maxConnected atomic.Int64

	mu       sync.Mutex
	ttfb     []time.Duration // request start -> first body byte, every sample
	perConn  map[int64]*connStat
	failLvl  int64 // intended level at first failure signal, 0 = none yet
	failWhen time.Duration
	failWhy  string
}

func newStats() *stats {
	return &stats{perConn: make(map[int64]*connStat), failLvl: 0}
}

func (s *stats) register(id int64, cs *connStat) {
	s.mu.Lock()
	s.perConn[id] = cs
	s.mu.Unlock()
}

func (s *stats) unregister(id int64) {
	s.mu.Lock()
	delete(s.perConn, id)
	s.mu.Unlock()
}

func (s *stats) addTTFB(d time.Duration) {
	s.mu.Lock()
	s.ttfb = append(s.ttfb, d)
	s.mu.Unlock()
}

func (s *stats) noteFailure(when time.Duration, level int64, why string) {
	s.mu.Lock()
	if s.failLvl == 0 {
		s.failLvl = level
		s.failWhen = when
		s.failWhy = why
	}
	s.mu.Unlock()
}

// pacer replicates the server's own bitrate pacing (see internal/stream/autodj.go):
// after consuming n bytes it sleeps until wall-clock catches up to sent/rate.
type pacer struct {
	enabled bool
	rate    float64
	start   time.Time
	sent    int64
}

func newPacer(rate int, enabled bool) *pacer {
	return &pacer{enabled: enabled, rate: float64(rate), start: time.Now()}
}

func (p *pacer) consume(n int) {
	if !p.enabled || p.rate <= 0 {
		return
	}
	p.sent += int64(n)
	expected := time.Duration(float64(p.sent) / p.rate * float64(time.Second))
	if elapsed := time.Since(p.start); expected > elapsed {
		time.Sleep(expected - elapsed)
	}
}

func worker(ctx context.Context, id int64, cfg config, client *http.Client, st *stats) {
	cs := &connStat{}
	st.register(id, cs)
	defer st.unregister(id)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.url, nil)
	if err != nil {
		st.connectErrs.Add(1)
		return
	}
	req.Header.Set("User-Agent", "radio-loadtest/1.0")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			st.connectErrs.Add(1)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		st.connectErrs.Add(1)
		return
	}

	st.connected.Add(1)
	cs.alive.Store(true)
	defer func() {
		st.connected.Add(-1)
		cs.alive.Store(false)
	}()

	buf := make([]byte, 32*1024)
	pacer := newPacer(cfg.rate, cfg.realtime)
	firstByte := false
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if !firstByte {
				firstByte = true
				cs.firstNS.Store(time.Now().UnixNano())
				st.addTTFB(time.Since(start))
			}
			cs.bytes.Add(int64(n))
			st.totalBytes.Add(int64(n))
			pacer.consume(n)
		}
		if rerr != nil {
			classifyClose(ctx, rerr, st)
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func classifyClose(ctx context.Context, rerr error, st *stats) {
	if ctx.Err() != nil || errors.Is(rerr, context.Canceled) {
		return // deliberate end of test
	}
	st.earlyClosed.Add(1)
	msg := rerr.Error()
	switch {
	case errors.Is(rerr, io.ErrUnexpectedEOF) || strings.Contains(msg, "unexpected EOF"):
		st.closeReset.Add(1)
	case strings.Contains(msg, "connection reset"), strings.Contains(msg, "broken pipe"):
		st.closeReset.Add(1)
	case errors.Is(rerr, io.EOF):
		// A chunked infinite stream that ends cleanly mid-test means the server
		// stopped writing to us: slow-listener eviction, studio removal, or crash.
		st.closeEnded.Add(1)
	default:
		st.closeOther.Add(1)
	}
}

type sample struct {
	elapsed                       time.Duration
	intended, connected           int64
	connectErrs                   int64
	earlyClosed                   int64
	closeEnded, closeReset, other int64
	aggMBps                       float64
	ttfbP50, ttfbP95, ttfbP99     time.Duration
	perConnMinKBps, perConnMean   float64
}

func runReporter(ctx context.Context, cfg config, st *stats, done chan<- struct{}) {
	defer close(done)

	var w *csv.Writer
	if cfg.out != "" {
		f, err := os.Create(cfg.out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadtest: cannot create -out %s: %v\n", cfg.out, err)
		} else {
			defer f.Close()
			w = csv.NewWriter(f)
			_ = w.Write([]string{
				"elapsed_s", "intended", "connected", "connect_errs", "early_closed",
				"close_ended", "close_reset", "close_other", "agg_mbps",
				"ttfb_p50_ms", "ttfb_p95_ms", "ttfb_p99_ms", "perconn_min_kbps", "perconn_mean_kbps",
			})
			w.Flush()
		}
	}

	t0 := time.Now()
	tick := time.NewTicker(cfg.reportEvery)
	defer tick.Stop()

	var lastBytes int64
	var lastAt = t0
	var prevEarly, prevErrs int64

	emit := func() {
		now := time.Now()
		s := collect(st, now, t0, lastBytes, lastAt)
		lastBytes = st.totalBytes.Load()
		lastAt = now

		if c := s.connected; c > st.maxConnected.Load() {
			st.maxConnected.Store(c)
		}

		// Failure-signal detection: new connect errors, new early closes, or a
		// live listener starved below 90% of the target drain rate.
		underrunFloor := float64(cfg.rate) * 0.9 / 1024.0
		newErrs := s.connectErrs - prevErrs
		newEarly := s.earlyClosed - prevEarly
		prevErrs, prevEarly = s.connectErrs, s.earlyClosed
		switch {
		case newErrs > 0:
			st.noteFailure(s.elapsed, s.intended, fmt.Sprintf("%d new connect errors", newErrs))
		case newEarly > 0:
			st.noteFailure(s.elapsed, s.intended, fmt.Sprintf("%d listeners dropped by server", newEarly))
		case s.connected > 0 && cfg.realtime && s.perConnMinKBps > 0 && s.perConnMinKBps < underrunFloor:
			st.noteFailure(s.elapsed, s.intended, fmt.Sprintf("audio underrun: slowest listener %.1f KB/s < %.1f", s.perConnMinKBps, underrunFloor))
		}

		fmt.Fprintf(os.Stderr,
			"[%6.0fs] intended=%-6d connected=%-6d cerr=%-4d dropped=%-4d(ended=%d reset=%d other=%d) agg=%6.1f MB/s ttfb50=%-5s p95=%-6s minKB/s=%6.1f meanKB/s=%6.1f\n",
			s.elapsed.Seconds(), s.intended, s.connected, s.connectErrs, s.earlyClosed,
			s.closeEnded, s.closeReset, s.other, s.aggMBps,
			s.ttfbP50.Round(time.Millisecond), s.ttfbP95.Round(time.Millisecond),
			s.perConnMinKBps, s.perConnMean)

		if w != nil {
			_ = w.Write([]string{
				strconv.FormatFloat(s.elapsed.Seconds(), 'f', 0, 64),
				strconv.FormatInt(s.intended, 10),
				strconv.FormatInt(s.connected, 10),
				strconv.FormatInt(s.connectErrs, 10),
				strconv.FormatInt(s.earlyClosed, 10),
				strconv.FormatInt(s.closeEnded, 10),
				strconv.FormatInt(s.closeReset, 10),
				strconv.FormatInt(s.other, 10),
				strconv.FormatFloat(s.aggMBps, 'f', 2, 64),
				strconv.FormatFloat(float64(s.ttfbP50.Microseconds())/1000, 'f', 1, 64),
				strconv.FormatFloat(float64(s.ttfbP95.Microseconds())/1000, 'f', 1, 64),
				strconv.FormatFloat(float64(s.ttfbP99.Microseconds())/1000, 'f', 1, 64),
				strconv.FormatFloat(s.perConnMinKBps, 'f', 1, 64),
				strconv.FormatFloat(s.perConnMean, 'f', 1, 64),
			})
			w.Flush()
		}
	}

	for {
		select {
		case <-ctx.Done():
			if time.Since(lastAt) > cfg.reportEvery/4 {
				emit() // final row, unless a tick just fired
			}
			return
		case <-tick.C:
			emit()
		}
	}
}

func collect(st *stats, now, t0 time.Time, lastBytes int64, lastAt time.Time) sample {
	st.mu.Lock()
	ttfb := make([]time.Duration, len(st.ttfb))
	copy(ttfb, st.ttfb)
	kbps := make([]float64, 0, len(st.perConn))
	for _, cs := range st.perConn {
		if !cs.alive.Load() {
			continue
		}
		fn := cs.firstNS.Load()
		if fn == 0 {
			continue
		}
		el := now.Sub(time.Unix(0, fn)).Seconds()
		if el <= 0 {
			continue
		}
		kbps = append(kbps, float64(cs.bytes.Load())/1024.0/el)
	}
	st.mu.Unlock()

	sort.Slice(ttfb, func(i, j int) bool { return ttfb[i] < ttfb[j] })

	totalBytes := st.totalBytes.Load()
	dt := now.Sub(lastAt).Seconds()
	var agg float64
	if dt > 0 {
		agg = float64(totalBytes-lastBytes) / 1e6 / dt
	}

	var minK, meanK float64
	if len(kbps) > 0 {
		minK = kbps[0]
		var sum float64
		for _, v := range kbps {
			if v < minK {
				minK = v
			}
			sum += v
		}
		meanK = sum / float64(len(kbps))
	}

	return sample{
		elapsed:        now.Sub(t0),
		intended:       st.intended.Load(),
		connected:      st.connected.Load(),
		connectErrs:    st.connectErrs.Load(),
		earlyClosed:    st.earlyClosed.Load(),
		closeEnded:     st.closeEnded.Load(),
		closeReset:     st.closeReset.Load(),
		other:          st.closeOther.Load(),
		aggMBps:        agg,
		ttfbP50:        percentile(ttfb, 50),
		ttfbP95:        percentile(ttfb, 95),
		ttfbP99:        percentile(ttfb, 99),
		perConnMinKBps: minK,
		perConnMean:    meanK,
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// raiseFDLimit best-effort bumps RLIMIT_NOFILE toward its hard max so the
// generator can open tens of thousands of sockets. Failures are non-fatal.
func raiseFDLimit() {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return
	}
	const want = 1 << 20 // 1,048,576 is plenty and avoids RLIM_INFINITY noise
	target := min(lim.Max, want)
	if lim.Cur >= target {
		fmt.Fprintf(os.Stderr, "loadtest: fd limit = %d\n", lim.Cur)
		return
	}
	old := lim.Cur
	lim.Cur = target
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: fd limit stays at %d (raise it manually with `ulimit -n`): %v\n", old, err)
		return
	}
	fmt.Fprintf(os.Stderr, "loadtest: raised fd limit %d -> %d\n", old, lim.Cur)
}

func main() {
	cfg := parseFlags()
	raiseFDLimit()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "loadtest: interrupted, tearing down connections...")
		cancel()
	}()

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxConnsPerHost:       0,
		DisableCompression:    true,
		DisableKeepAlives:     true, // one dedicated connection per virtual listener
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: cfg.dialTimeout,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: cfg.insecure},
	}
	client := &http.Client{Timeout: 0, Transport: transport}

	st := newStats()
	repDone := make(chan struct{})
	repCtx, repCancel := context.WithCancel(context.Background())
	go runReporter(repCtx, cfg, st, repDone)

	var wg sync.WaitGroup
	var nextID atomic.Int64
	spawn := func(count int) {
		for i := 0; i < count; i++ {
			if ctx.Err() != nil {
				return
			}
			id := nextID.Add(1)
			st.intended.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				worker(ctx, id, cfg, client, st)
			}()
			if cfg.spawnDelay > 0 {
				time.Sleep(cfg.spawnDelay)
			}
		}
	}

	testStart := time.Now()
	fmt.Fprintf(os.Stderr, "loadtest: %s | start=%d step=%d/%s max=%d hold=%s drain=%s rate=%d B/s\n",
		cfg.url, cfg.start, cfg.step, cfg.stepInterval, cfg.max, cfg.hold,
		map[bool]string{true: "realtime", false: "fast"}[cfg.realtime], cfg.rate)

	current := 0
	first := cfg.start
	if first > cfg.max {
		first = cfg.max
	}
	spawn(first)
	current = first

	if cfg.step > 0 && cfg.stepInterval > 0 {
		tick := time.NewTicker(cfg.stepInterval)
	rampLoop:
		for current < cfg.max {
			select {
			case <-ctx.Done():
				break rampLoop
			case <-tick.C:
				add := cfg.step
				if current+add > cfg.max {
					add = cfg.max - current
				}
				spawn(add)
				current += add
			}
		}
		tick.Stop()
	}

	if ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "loadtest: at max (%d), holding %s\n", current, cfg.hold)
		select {
		case <-ctx.Done():
		case <-time.After(cfg.hold):
		}
	}

	cancel()
	wg.Wait()
	repCancel()
	<-repDone

	printSummary(cfg, st, time.Since(testStart))
}

func printSummary(cfg config, st *stats, elapsed time.Duration) {
	st.mu.Lock()
	failLvl, failWhen, failWhy := st.failLvl, st.failWhen, st.failWhy
	st.mu.Unlock()

	egressMBps := float64(st.maxConnected.Load()) * float64(cfg.rate) / 1e6

	fmt.Fprintln(os.Stderr, "\n──────── summary ────────")
	fmt.Fprintf(os.Stderr, "duration:                 %s\n", elapsed.Round(time.Second))
	fmt.Fprintf(os.Stderr, "peak concurrent streams:  %d\n", st.maxConnected.Load())
	fmt.Fprintf(os.Stderr, "intended peak:            %d\n", st.intended.Load())
	fmt.Fprintf(os.Stderr, "connect errors:           %d\n", st.connectErrs.Load())
	fmt.Fprintf(os.Stderr, "dropped by server:        %d (ended=%d reset=%d other=%d)\n",
		st.earlyClosed.Load(), st.closeEnded.Load(), st.closeReset.Load(), st.closeOther.Load())
	fmt.Fprintf(os.Stderr, "approx egress at peak:    %.1f MB/s (%.0f kbps/listener)\n",
		egressMBps, float64(cfg.rate)*8/1000)
	if failLvl == 0 {
		fmt.Fprintf(os.Stderr, "result:                   NO failure signal up to %d listeners\n", st.maxConnected.Load())
	} else {
		fmt.Fprintf(os.Stderr, "result:                   first failure at intended=%d (t+%s): %s\n",
			failLvl, failWhen.Round(time.Second), failWhy)
	}
	fmt.Fprintln(os.Stderr, "─────────────────────────")
}
