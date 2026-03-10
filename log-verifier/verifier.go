package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/VictoriaLogs/lib/logstorage"
	"github.com/VictoriaMetrics/metrics"
)

// verifier implements logstorage.LogRowsStorage and verifies that all logs are received by the log generator.
type verifier struct {
	lock         *sync.RWMutex
	sourcesStats map[sourceKey]*sourceStats
}

var storage = &verifier{
	lock:         &sync.RWMutex{},
	sourcesStats: map[sourceKey]*sourceStats{},
}

// MustAddRows implements logstorage.LogRowsStorage.
// It groups incoming log rows by source (collector + pod) and adds them
// to the corresponding sourceStats in a single batch to minimize lock contention.
func (s *verifier) MustAddRows(lr *logstorage.LogRows) {
	current := time.Now().UnixNano()

	batches := make(map[sourceKey]*bufferedEntries)
	lr.ForEachRow(func(_ uint64, r *logstorage.InsertRow) {
		key, entry, ok := parseRow(r.Fields, current)
		if !ok {
			return
		}
		if batches[key] == nil {
			batches[key] = getBufferedEntries()
		}
		batches[key].add(entry)
	})
	for key, entries := range batches {
		s.getOrCreateSourceStats(key).add(entries.s)
		putBufferedEntries(entries)
	}
}

type sourceKey struct {
	collector string
	generator string
}

type bufferedEntry struct {
	id          uint64
	receivedAt  int64
	generatedAt int64
}

func parseRow(row []logstorage.Field, current int64) (sourceKey, bufferedEntry, bool) {
	collector, ok := getFieldValue(row, "collector")
	if !ok {
		panic(fmt.Errorf("BUG: 'collector' field must be set; ensure that every log collector sets VL-Extra-Fields header"))
	}

	sequenceIDStr, ok := getFieldValue(row, "sequence_id")
	if !ok {
		metrics.GetOrCreateCounter(fmt.Sprintf(`log_verifier_malformed_logs_total{collector=%q, reason=%q}`, collector, "missing_sequence_id")).Inc()
		return sourceKey{}, bufferedEntry{}, false
	}
	sequenceID, err := strconv.ParseUint(sequenceIDStr, 10, 64)
	if err != nil {
		metrics.GetOrCreateCounter(fmt.Sprintf(`log_verifier_malformed_logs_total{collector=%q, reason=%q}`, collector, "invalid_sequence_id")).Inc()
		return sourceKey{}, bufferedEntry{}, false
	}

	generatedAtStr, ok := getFieldValue(row, "generated_at")
	if !ok {
		metrics.GetOrCreateCounter(fmt.Sprintf(`log_verifier_malformed_logs_total{collector=%q, reason=%q}`, collector, "missing_generated_at")).Inc()
		return sourceKey{}, bufferedEntry{}, false
	}
	generatedAt, err := strconv.ParseInt(generatedAtStr, 10, 64)
	if err != nil {
		metrics.GetOrCreateCounter(fmt.Sprintf(`log_verifier_malformed_logs_total{collector=%q, reason=%q}`, collector, "invalid_generated_at")).Inc()
		return sourceKey{}, bufferedEntry{}, false
	}

	n := slices.IndexFunc(row, func(f logstorage.Field) bool {
		switch f.Name {
		case "kubernetes.pod_name", "pod", "kubernetes.pod.name", "k8s.pod.name":
			return true
		default:
			return false
		}
	})
	if n < 0 {
		metrics.GetOrCreateCounter(fmt.Sprintf(`log_verifier_malformed_logs_total{collector=%q, reason=%q}`, collector, "missing_pod_name")).Inc()
		return sourceKey{}, bufferedEntry{}, false
	}
	generator := row[n].Value

	key := sourceKey{
		collector: collector,
		generator: generator,
	}
	entry := bufferedEntry{
		id:          sequenceID,
		receivedAt:  current,
		generatedAt: generatedAt,
	}
	return key, entry, true
}

func (s *verifier) getOrCreateSourceStats(key sourceKey) *sourceStats {
	s.lock.RLock()
	ss, ok := s.sourcesStats[key]
	s.lock.RUnlock()
	if ok {
		// Fast path: the source already exists.
		return ss
	}

	// Slow path: source not yet created.
	s.lock.Lock()
	defer s.lock.Unlock()
	if ss, ok = s.sourcesStats[key]; ok {
		return ss
	}

	ss = newSourceStats(key.collector, key.generator)
	s.sourcesStats[sourceKey{
		collector: strings.Clone(key.collector),
		generator: strings.Clone(key.generator),
	}] = ss
	return ss
}

type sourceStats struct {
	maxSeqID       *atomic.Uint64
	latencySeconds *metrics.Histogram
	total          *metrics.Counter
}

func newSourceStats(collector, generator string) *sourceStats {
	maxSeqID := &atomic.Uint64{}
	loadMaxSeqID := func() float64 {
		return float64(maxSeqID.Load())
	}
	metrics.GetOrCreateGauge(fmt.Sprintf(`log_verifier_max_sequence_id{collector=%q, log_generator_pod=%q}`, collector, generator), loadMaxSeqID)

	return &sourceStats{
		maxSeqID:       maxSeqID,
		latencySeconds: metrics.GetOrCreateHistogram(fmt.Sprintf(`log_verifier_delivery_latency_seconds{collector=%q, log_generator_pod=%q}`, collector, generator)),
		total:          metrics.GetOrCreateCounter(fmt.Sprintf(`log_verifier_logs_total{collector=%q, log_generator_pod=%q}`, collector, generator)),
	}
}

// add buffers to the incoming sequence ID with the current timestamp.
func (ss *sourceStats) add(entries []bufferedEntry) {
	ss.total.Add(len(entries))

	maxSeqID := uint64(0)
	for i := range entries {
		e := &entries[i]

		delay := e.receivedAt - e.generatedAt
		ss.latencySeconds.Update(float64(delay) / 1e9)

		if maxSeqID < e.id {
			maxSeqID = e.id
		}
	}

	atomicCASMax(ss.maxSeqID, maxSeqID)
}

func (s *verifier) CanWriteData() error {
	return nil
}

func getFieldValue(fields []logstorage.Field, name string) (string, bool) {
	n := slices.IndexFunc(fields, func(f logstorage.Field) bool {
		return f.Name == name
	})
	if n < 0 {
		return "", false
	}
	return fields[n].Value, true
}

type bufferedEntries struct {
	s []bufferedEntry
}

func (bes *bufferedEntries) add(entry bufferedEntry) {
	bes.s = append(bes.s, entry)
}

func (bes *bufferedEntries) reset() {
	bes.s = bes.s[:0]
}

var bufferedEntriesPool = sync.Pool{
	New: func() any {
		return &bufferedEntries{s: make([]bufferedEntry, 0, 1024)}
	},
}

func getBufferedEntries() *bufferedEntries {
	return bufferedEntriesPool.Get().(*bufferedEntries)
}

func putBufferedEntries(buf *bufferedEntries) {
	buf.reset()
	bufferedEntriesPool.Put(buf)
}

func atomicCASMax(target *atomic.Uint64, v uint64) {
	for {
		old := target.Load()
		if old >= v {
			break
		}
		if target.CompareAndSwap(old, v) {
			break
		}
	}
}
