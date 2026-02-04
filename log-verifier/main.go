package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

var (
	victoriaLogsAddr = flag.String("victoriaLogsAddr", "http://localhost:9428", "")
	listenAddr       = flag.String("listenAddr", ":8080", "")
)

func main() {
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}

	http.DefaultClient.Timeout = 10 * time.Second
	http.DefaultTransport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   1 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        10,
		IdleConnTimeout:     10 * time.Second,
		MaxConnsPerHost:     100,
		MaxIdleConnsPerHost: 100,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go mustStartDebugServer(*listenAddr)

	for {
		launchStatsWorkers()

		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

var runningWorkers = map[workerKey]struct{}{}

type workerKey struct {
	logGeneratorPod string
	collector       string
}

func launchStatsWorkers() {
	query := "_time:5m | uniq by (collector)"
	var collectors []string
	mustSendVictoriaLogsQuery(query, func(line []byte) {
		resp := struct {
			Collector string `json:"collector"`
		}{}
		mustUnmarshal(line, &resp)
		collectors = append(collectors, resp.Collector)
	})

	query = "_time:5m kubernetes.container_name:=log-generator | uniq by (kubernetes.pod_name)"
	var generatorPods []string
	mustSendVictoriaLogsQuery(query, func(line []byte) {
		resp := struct {
			Pod string `json:"kubernetes.pod_name"`
		}{}
		mustUnmarshal(line, &resp)
		generatorPods = append(generatorPods, resp.Pod)
	})

	if len(collectors) == 0 || len(generatorPods) == 0 {
		slog.Info("no collectors or log-generator pods found", "collectors", collectors, "log_generators", generatorPods)
		return
	}

	for _, collector := range collectors {
		for _, pod := range generatorPods {
			key := workerKey{collector: collector, logGeneratorPod: pod}
			if _, ok := runningWorkers[key]; ok {
				continue
			}
			runningWorkers[key] = struct{}{}

			slog.Info("starting worker", "collector", collector, "log_generator", pod)

			go startUpdateStats(collector, pod)
		}
	}
}

func startUpdateStats(collector, logGeneratorPod string) {
	missed := metrics.GetOrCreateCounter(fmt.Sprintf(`log_verifier_missed_logs_total{log_generator_pod=%q, collector=%q}`, logGeneratorPod, collector))
	duplicated := metrics.GetOrCreateCounter(fmt.Sprintf(`log_verifier_duplicated_logs_total{log_generator_pod=%q, collector=%q}`, logGeneratorPod, collector))
	total := metrics.GetOrCreateCounter(fmt.Sprintf(`log_verifier_logs_total{log_generator_pod=%q, collector=%q}`, logGeneratorPod, collector))

	healthy := atomic.Uint32{}
	_ = metrics.GetOrCreateGauge(fmt.Sprintf(`log_verifier_healthy{log_generator_pod=%q, collector=%q}`, logGeneratorPod, collector), func() float64 {
		return float64(healthy.Load())
	})

	// Window for querying sequence IDs.
	const queryWindow = time.Minute * 5

	// Offset to give collectors time to process the logs.
	const offset = time.Minute

	const checkInterval = time.Second * 10

	// seqID used as a cursor for the next query.
	seqID := uint64(0)

	for {
		now := time.Now().UTC()
		from := now.Add(-queryWindow - offset)
		to := now.Add(-offset)

		stats := exportLogGeneratorStats(collector, logGeneratorPod, seqID, from, to)

		if stats.lastSeqID != 0 {
			seqID = stats.lastSeqID
		}

		duplicated.Add(stats.duplicatedLogs)
		missed.Add(stats.missedLogs)
		total.Add(stats.totalLogs)

		if stats.totalLogs == 0 {
			healthy.Store(0)
		} else {
			healthy.Store(1)
		}

		// Add jitter to avoid the thundering herd problem
		<-time.After(checkInterval + rand.N(checkInterval/10))
	}
}

type stats struct {
	duplicatedLogs int
	missedLogs     int
	totalLogs      int
	lastSeqID      uint64
}

func exportLogGeneratorStats(collector, generatorPod string, prevSeqID uint64, from, to time.Time) stats {
	var duplicates int
	duplicatesInfo := map[uint64]int{}
	var misses int

	seqIDs := exportSeqIDs(collector, generatorPod, prevSeqID, from, to)
	if len(seqIDs) == 0 {
		return stats{}
	}

	if prevSeqID == 0 {
		prevSeqID = seqIDs[0] - 1
	}

	for _, seqID := range seqIDs {
		if seqID < prevSeqID {
			panic(fmt.Errorf("sequence_id is not monotonically increasing: %d < %d", prevSeqID, seqID))
		}

		if seqID == prevSeqID {
			duplicates++
			duplicatesInfo[seqID]++
		} else if seqID-1 == prevSeqID {
			// OK
		} else {
			n := int(seqID - prevSeqID - 1)
			misses += n
			slog.Info("logs missed",
				"collector", collector,
				"log_generator_pod", generatorPod,
				"missed_count", n,
				"query_range_from", from.Format(time.RFC3339Nano),
				"query_range_to", to.Format(time.RFC3339Nano),
				"seq_id_range_start", prevSeqID+1,
				"seq_id_range_end", seqID-1)
		}

		prevSeqID = seqID
	}

	if len(duplicatesInfo) > 0 {
		slog.Info("duplicated logs", "duplicates", duplicatesInfo, "collector", collector, "log_generator_pod", generatorPod)
	}

	return stats{
		duplicatedLogs: duplicates,
		missedLogs:     misses,
		totalLogs:      len(seqIDs),
		lastSeqID:      prevSeqID,
	}
}

func exportSeqIDs(collector, generatorPod string, minSeqID uint64, from, to time.Time) []uint64 {
	var seqIDs []uint64
	pushSeqID := func(line []byte) {
		resp := struct {
			SequenceID json.Number `json:"sequence_id"`
		}{}
		mustUnmarshal(line, &resp)
		v, err := resp.SequenceID.Int64()
		if err != nil {
			panic(fmt.Errorf("cannot parse sequence_id=%q: %w", resp.SequenceID, err))
		}
		seqIDs = append(seqIDs, uint64(v))
	}

	podFieldName := ""
	switch collector {
	case "vector", "vlagent", "fluentbit", "alloy", "fluentd", "promtail":
		podFieldName = "kubernetes.pod_name"
	case "grafana-agent":
		podFieldName = "pod"
	case "filebeat":
		podFieldName = "kubernetes.pod.name"
	case "opentelemetry":
		podFieldName = "k8s.pod.name"
	default:
		slog.Warn("unknown collector", "collector", collector)
		return nil
	}

	query := fmt.Sprintf(`_time:[%q, %q] {%q=%q, collector=%q} sequence_id:>%d | sort by (sequence_id asc) | keep sequence_id`,
		from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), podFieldName, generatorPod, collector, minSeqID)
	mustSendVictoriaLogsQuery(query, pushSeqID)

	return seqIDs
}

func mustSendVictoriaLogsQuery(query string, cb func(line []byte)) {
	if err := sendVictoriaLogsQueryInternal(query, cb, true); err != nil {
		panic(err)
	}
}

func sendVictoriaLogsQueryInternal(q string, cb func(line []byte), allowRetry bool) error {
	addr := *victoriaLogsAddr
	addr = strings.TrimRight(addr, "/")

	start := time.Now()
	defer func() {
		took := time.Since(start)
		if took > 5*time.Second {
			slog.Warn("slow query", "query", q, "took", took)
		}
	}()

	resp, err := http.PostForm(addr+"/select/logsql/query", url.Values{"query": {q}})
	if err != nil {
		// VictoriaLogs sometimes returns 'read: connection reset by peer'.
		// Retrying in this case should help.
		if allowRetry {
			return sendVictoriaLogsQueryInternal(q, cb, false)
		}
		return fmt.Errorf("cannot send query %q: %w", q, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code: %d; response: %q", resp.StatusCode, payload)
	}

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		cb(sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("cannot scan response: %w", err)
	}
	return nil
}

func mustStartDebugServer(listenAddr string) {
	handle := func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		metrics.WritePrometheus(rw, true)
	}

	slog.Info("starting debug server", "addr", listenAddr)
	if err := http.ListenAndServe(listenAddr, http.HandlerFunc(handle)); err != nil {
		panic(err)
	}
}

func mustUnmarshal(data []byte, v any) {
	if err := json.Unmarshal(data, v); err != nil {
		panic(fmt.Errorf("cannot unmarshal %q into %T: %w", data, v, err))
	}
}
