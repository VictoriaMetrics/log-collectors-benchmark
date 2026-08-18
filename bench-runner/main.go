package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

type collectorInfo struct {
	Protocol    string
	Compression string
}

// collectors contains all supported log collectors with their delivery configuration,
// which was observed manually by modifying log-verificator source code.
var collectors = map[string]collectorInfo{
	"vlagent":                 {Protocol: "JSON Lines", Compression: "zstd"},
	"vector":                  {Protocol: "JSON Lines", Compression: "zstd"},
	"promtail":                {Protocol: "Loki", Compression: "snappy"},
	"alloy":                   {Protocol: "Loki", Compression: "snappy"},
	"grafana-agent":           {Protocol: "Loki", Compression: "snappy"},
	"fluent-bit":              {Protocol: "JSON Lines", Compression: "gzip"},
	"opentelemetry-collector": {Protocol: "OpenTelemetry", Compression: "gzip"},
	"filebeat":                {Protocol: "Elasticsearch", Compression: "none"},
	"fluentd":                 {Protocol: "JSON Lines", Compression: "gzip"},
}

// benchmarkTimeout maximum time to wait until a collector reaches its maximum throughput.
//
// Maximum duration for the whole benchmark run is benchmarkTimeout * len(allCollectors).
const benchmarkTimeout = time.Hour

// case10kPerSecDuration is duration of the "10k logs per second" benchmark case.
// After this duration benchmark considered as finished.
const case10kPerSecDuration = time.Hour

var (
	collectorsFlag = flag.String("collectors", "", "Comma-separated list of collectors to run. By default, all collectors will be run")
)

func main() {
	flag.Parse()

	args := flag.Args()
	if len(args) > 0 && args[0] == "render" {
		doc := buildIndexHTML()
		_, _ = os.Stdout.WriteString(doc)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Ensure monitoring is up.
	sh(ctx, "make", "bench-up-monitoring")
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		sh(cleanupCtx, "make", "bench-down-all")
	}()

	// Ensure collectors are not running.
	sh(ctx, "make", "bench-down-collectors")

	start := time.Now()

	// Write info.json
	writeRunInfo(start)

	toBench := getCollectorsToBench()

	// Measure maximum throughput across all collectors.
	var maxThroughputReports []collectorReport
	for _, collector := range toBench {
		storedReport, ok := tryReadReportForDate(start, collector, "throughput")
		if ok && storedReport.Throughput > 0 {
			maxThroughputReports = append(maxThroughputReports, storedReport)
			slog.Warn("skipping maximum throughput benchmark since report already exists", slog.String("collector", collector))
			continue
		}

		report, err := runThroughputCase(ctx, collector)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fatalf("interrupted")
			}
			slog.Warn("failed to run maximum throughput benchmark", "collector", collector, "err", err)
			continue
		}

		writeReportToFile(start, report, "throughput")
		maxThroughputReports = append(maxThroughputReports, report)
	}

	// Measure resource usage, running log-generator with static throughput that each collector can process.
	for _, collector := range toBench {
		n := slices.IndexFunc(maxThroughputReports, func(r collectorReport) bool {
			return r.Collector == collector
		})
		if n < 0 {
			slog.Warn("skipping collector from 10k/sec benchmark, since it didn't pass max-throughput benchmark",
				slog.String("collector", collector))
			continue
		}
		maxThroughput := maxThroughputReports[n].Throughput
		if maxThroughput < 11_000 {
			slog.Warn("skipping collector from 10k/sec benchmark, since maximum throughput too low",
				slog.String("collector", collector), slog.Int("throughput", maxThroughput))
			continue
		}

		storedReport, ok := tryReadReportForDate(start, collector, "resources")
		if ok && storedReport.Throughput > 0 {
			slog.Warn("skipping 10k/sec benchmark since report already exists", slog.String("collector", collector))
			continue
		}

		report, err := runResourcesCase(ctx, collector)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fatalf("interrupted")
			}
			slog.Warn("failed to run resource usage benchmark", "collector", collector, "err", err)
			continue
		}
		writeReportToFile(start, report, "resources")
	}

	slog.Info("benchmark completed for all benchmark cases", slog.Duration("duration", time.Since(start)))
}

func getCollectorsToBench() []string {
	var allCollectors []string
	for name := range collectors {
		allCollectors = append(allCollectors, name)
	}

	if *collectorsFlag != "" {
		toBench := strings.Split(*collectorsFlag, ",")
		for _, collector := range toBench {
			if !slices.Contains(allCollectors, collector) {
				fatalf("unknown collector %q", collector)
			}
		}
		return toBench
	}

	toBench := allCollectors
	// Explicitly shuffle collectors.
	rand.Shuffle(len(toBench), func(i, j int) {
		toBench[i], toBench[j] = toBench[j], toBench[i]
	})
	return toBench
}

func runThroughputCase(ctx context.Context, collector string) (collectorReport, error) {
	benchmark := func() error {
		opts := logGeneratorOpts{
			// Start with 50 logs/sec (per replica).
			logPerSecond: 50,
			// Increase load for 20 logs/sec each 10 sec.
			rampUp:             true,
			rampUpStep:         20,
			rampUpStepInterval: time.Second * 10,
			// Run 50 replicas.
			replicas: 50,
		}
		stopGenerator := runLogGenerator(ctx, opts)
		defer stopGenerator()

		if err := waitForSaturation(ctx, collector); err != nil {
			return err
		}
		return nil
	}
	return runBenchCaseGeneric(ctx, collector, benchmark)
}

type logGeneratorOpts struct {
	logPerSecond int

	rampUp             bool
	rampUpStep         int
	rampUpStepInterval time.Duration

	replicas int
}

func runLogGenerator(ctx context.Context, opts logGeneratorOpts) func() {
	sh(ctx, "make", "bench-up-generator",
		fmt.Sprintf("LOGS_PER_SECOND=%d", opts.logPerSecond),
		fmt.Sprintf("RAMP_UP=%v", opts.rampUp),
		fmt.Sprintf("RAMP_UP_STEP=%d", opts.rampUpStep),
		fmt.Sprintf("RAMP_UP_STEP_INTERVAL=%s", opts.rampUpStepInterval),
		fmt.Sprintf("GENERATOR_REPLICAS=%d", opts.replicas),
	)
	return func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		sh(cleanupCtx, "make", "bench-down-generator")
	}
}

func waitForSaturation(ctx context.Context, collector string) error {
	slog.Info("wait for collector saturation", slog.String("collector", collector))

	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	// Expected growth per check: rampUp=2/sec * 30 sec * replicas=50 = 3000 logs.
	// 1000 was determined empirically and ensures the collector cannot keep up with the load.
	const expectedGrows = 1000

	start := time.Now()

	var prevReport *collectorReport
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if time.Since(start) > benchmarkTimeout {
			return fmt.Errorf("not saturated in %s", benchmarkTimeout)
		}

		report, err := collectReport(ctx, collector, start)
		if err != nil {
			return err
		}
		if !report.Success {
			// Collector has failed.
			return nil
		}

		if prevReport == nil && report.Throughput == 0 {
			slog.Info("no data yet", slog.String("collector", collector))
			continue
		}

		if prevReport == nil {
			prevReport = &report
			continue
		}

		growth := report.Throughput - prevReport.Throughput
		if growth < expectedGrows {
			// Throughput stopped growing.
			slog.Info("saturated",
				slog.String("collector", collector),
				slog.Int("throughput", report.Throughput),
				slog.Int("growth", growth),
				slog.Duration("elapsed", time.Since(start)))
			return nil
		}
		prevReport = &report

		slog.Info("throughput increased",
			slog.String("collector", collector),
			slog.Int("throughput", report.Throughput),
			slog.Int("growth", growth),
			slog.Duration("elapsed", time.Since(start)))
	}
}

func runResourcesCase(ctx context.Context, collector string) (collectorReport, error) {
	benchmark := func() error {
		opts := logGeneratorOpts{
			logPerSecond: 1000,
			rampUp:       false,
			replicas:     10,
		}
		shutdown := runLogGenerator(ctx, opts)
		defer shutdown()

		if err := sleepCtx(ctx, case10kPerSecDuration); err != nil {
			return err
		}
		return nil
	}
	return runBenchCaseGeneric(ctx, collector, benchmark)
}

func runBenchCaseGeneric(ctx context.Context, collector string, benchFunc func() error) (collectorReport, error) {
	slog.Info("running benchmark for collector", slog.String("collector", collector))

	// Reset log-verifier state.
	sh(ctx, "kubectl", "rollout", "restart", "deployment/log-verifier", "-n", "monitoring")
	sh(ctx, "kubectl", "rollout", "status", "deployment/log-verifier", "-n", "monitoring", "--timeout=3m")

	// Start collector.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		sh(cleanupCtx, "make", "bench-down-"+collector)
	}()
	sh(ctx, "make", "bench-up-"+collector)

	// Wait collector to be ready.
	// Do not use kube_pod_status_ready metrics, since some
	// collectors are not ready until they process at least one log line.
	slog.Info("waiting collector to be ready", slog.String("collector", collector))
	if err := sleepCtx(ctx, time.Minute); err != nil {
		return collectorReport{}, err
	}

	start := time.Now()
	slog.Info("starting benchmark", slog.String("collector", collector))
	if err := benchFunc(); err != nil {
		return collectorReport{}, err
	}
	slog.Info("finished benchmark", slog.String("collector", collector), slog.Duration("duration", time.Since(start)))

	if err := sleepCtx(ctx, time.Minute); err != nil {
		return collectorReport{}, err
	}

	report, err := collectReport(ctx, collector, start)
	if err != nil {
		return collectorReport{}, err
	}
	return report, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func fatalf(s string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, s+"\n", args...)
	os.Exit(1)
}
