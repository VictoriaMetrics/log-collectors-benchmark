package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// resultsDir path to a folder with reports.
//
// This path will be used as a destination for reports
// and as a source to render final HTML.
const resultsDir = "./results"

type collectorReport struct {
	// Collector name.
	Collector string

	// Version of collector.
	Version string

	// Success true if benchmark case has failed.
	// See also FailReason which must be non-empty if Success is false.
	Success bool

	// FailReason reason why report is not Success.
	// E.g., "OOMKilled".
	FailReason string `json:"FailReason,omitempty"`

	// Throughput is the maximum throughput that collector reached in count per second.
	Throughput int

	// AvgCPUUsage
	AvgCPUUsage float64

	// MaxMemory usage in bytes.
	MaxMemory int

	// AvgMemory usage in bytes.
	AvgMemory int

	// NetworkBytesPerLog is a number of transmitted network bytes divided by total processed logs.
	NetworkBytesPerLog int

	// Lost is a number of logs that were produced by log-generator but log-verifier hasn't received them.
	Lost int
}

const folderNameFormat = "2006-01-02"

func tryReadReportForDate(date time.Time, collectorName, caseName string) (collectorReport, bool) {
	filename := fmt.Sprintf("%s-%s.json", collectorName, caseName)
	reportPath := filepath.Join(resultsDir, date.Format(folderNameFormat), filename)

	data, err := os.ReadFile(reportPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return collectorReport{}, false
		}
		fatalf("cannot read file %q: %s", reportPath, err)
	}
	var report collectorReport
	if err := json.Unmarshal(data, &report); err != nil {
		fatalf("cannot parse %q from %q: %s", data, reportPath, err)
	}
	return report, true
}

func writeReportToFile(date time.Time, report collectorReport, caseName string) {
	dstFolder := filepath.Join(resultsDir, date.Format(folderNameFormat))
	if err := os.MkdirAll(dstFolder, 0755); err != nil {
		fatalf("cannot create directory %q: %s", dstFolder, err)
	}
	filename := fmt.Sprintf("%s-%s.json", report.Collector, caseName)
	dstFile := filepath.Join(dstFolder, filename)

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		panic(err)
	}

	writeFileAtomic(dstFile, data)
}

func writeFileAtomic(dstFile string, data []byte) {
	tmp := dstFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		fatalf("failed to write to %q: %s", tmp, err)
	}
	if err := os.Rename(tmp, dstFile); err != nil {
		fatalf("failed to rename %q: %s", tmp, err)
	}
}

// readAvailableDates returns list of available dates to create a report.
// See also tryReadReportForDate.
func readAvailableDates() []time.Time {
	des, err := os.ReadDir(resultsDir)
	if err != nil {
		fatalf("failed to read reports directory: %s", err)
	}
	var dates []time.Time
	for _, de := range des {
		dateStr := de.Name()
		date, err := time.Parse(folderNameFormat, dateStr)
		if err != nil {
			// Not a folder with reports.
			continue
		}
		dates = append(dates, date)
	}
	sort.Slice(dates, func(i, j int) bool {
		return dates[i].Before(dates[j])
	})
	return dates
}

// collectReport creates report based on metrics stored in VictoriaMetrics.
func collectReport(ctx context.Context, collector string, benchStart time.Time) (collectorReport, error) {
	report := collectorReport{Collector: collector, Success: true}

	benchTime := time.Since(benchStart)
	if benchTime < time.Minute {
		benchTime = time.Minute
	}
	queryRange := fmt.Sprintf("%ds", benchTime/time.Second)

	// Fetch image version.
	query := fmt.Sprintf(`last_over_time(
	  kube_pod_container_info{namespace="collectors",pod=~"%s-.*",container!="config-reloader"}[%s]
	)`, collector, queryRange)
	vms, err := fetchVMMetrics(ctx, query)
	if err != nil {
		return report, err
	}
	if len(vms) > 0 {
		image := vms[0].labels["image"]
		if image == "" {
			panic(fmt.Errorf("BUG: no image found for collector %q", collector))
		}

		var version string
		if n := strings.LastIndexByte(image, ':'); n >= 0 {
			version = image[n+1:]
		}
		if n := strings.IndexByte(version, '-'); n >= 0 {
			// Remove any suffix after version number,
			// like "-debian-elasticsearch7-1" or "-distroless-libc".
			version = version[:n]
		}
		if version != "" && !strings.HasPrefix(version, "v") {
			version = "v" + version
		}
		if version == "" {
			panic(fmt.Errorf("BUG: no version found for collector %q", collector))
		}
		report.Version = version
	}

	query = fmt.Sprintf(`increase(kube_pod_container_status_restarts_total{namespace="collectors",pod=~"%s-.*"}[%s])
		* on(pod, container, namespace) group_left(reason)
		kube_pod_container_status_last_terminated_reason{namespace="collectors"} > 0`, collector, queryRange)
	vms, err = fetchVMMetrics(ctx, query)
	if err != nil {
		return report, err
	}
	if len(vms) > 0 {
		first := vms[0]
		report.Success = false
		report.FailReason = first.labels["reason"]
	}

	fetchMetricValue := func(q string) (float64, error) {
		mvs, err := fetchVMMetrics(ctx, q)
		if err != nil {
			return 0, err
		}
		if len(mvs) > 1 {
			panic(fmt.Errorf("BUG: got %d metrics instead of 1", len(mvs)))
		}
		if len(mvs) == 0 {
			return 0, nil
		}
		v := mvs[0].value
		if math.IsNaN(v) || math.IsInf(v, 0) {
			v = 0
		}
		return v, nil
	}

	// Fetch max throughput.
	//
	// This query uses subqueries to find maximum rate over time,
	// see https://docs.victoriametrics.com/victoriametrics/metricsql/#subqueries
	query = fmt.Sprintf(`max_over_time(
	  sum(rate(log_verifier_logs_total{collector=%q}[1m]))[%s:10s]
	)`, collector, queryRange)
	v, err := fetchMetricValue(query)
	if err != nil {
		return report, fmt.Errorf("cannot fetch throughput metric: %w", err)
	}
	report.Throughput = int(v)

	// Fetch max memory usage.
	query = fmt.Sprintf(`max_over_time(
		container_memory_rss{namespace="collectors",container!="",container!="config-reloader",image!="",pod=~"^%s-.*"}[%s]
	)`,
		collector, queryRange)
	v, err = fetchMetricValue(query)
	if err != nil {
		return report, fmt.Errorf("cannot fetch max memory metric: %w", err)
	}
	report.MaxMemory = int(v)

	// Fetch avg memory usage.
	query = fmt.Sprintf(`avg_over_time(
		container_memory_rss{namespace="collectors",container!="",container!="config-reloader",image!="",pod=~"^%s-.*"}[%s])`,
		collector, queryRange)
	v, err = fetchMetricValue(query)
	if err != nil {
		return report, fmt.Errorf("cannot fetch avg memory metric: %w", err)
	}
	report.AvgMemory = int(v)

	// Fetch network usage per 1 sent log.
	query = fmt.Sprintf(`
		sum(increase(container_network_transmit_bytes_total{namespace="collectors",pod=~"%s-.*"}[%s]))
		/
		sum(increase(log_verifier_logs_total{collector=%q}[%s]))`, collector, queryRange, collector, queryRange)
	v, err = fetchMetricValue(query)
	if err != nil {
		return report, fmt.Errorf("cannot fetch network bytes per log metric: %w", err)
	}
	report.NetworkBytesPerLog = int(v)

	// Fetch avg CPU usage.
	query = fmt.Sprintf(`avg_over_time(sum(rate(
		container_cpu_usage_seconds_total{namespace="collectors",container!="",container!="config-reloader",image!="",pod=~"^%s-.*"}[2m]
	))[%s:15s])`, collector, queryRange)
	v, err = fetchMetricValue(query)
	if err != nil {
		return report, fmt.Errorf("cannot fetch cpu usage metric: %w", err)
	}
	report.AvgCPUUsage = v

	query = fmt.Sprintf(`sum(log_verifier_max_sequence_id{collector=%q} - log_verifier_logs_total{collector=%q}) by (collector)`, collector, collector)
	v, err = fetchMetricValue(query)
	if err != nil {
		return report, fmt.Errorf("cannot fetch lost logs metric: %w", err)
	}
	report.Lost = int(v)

	return report, nil
}

var vmClient = &http.Client{Timeout: 30 * time.Second}

type metricValue struct {
	labels map[string]string
	value  float64
}

// fetchVMMetrics returns result fetched from vmURL by given query with current timestamp.
func fetchVMMetrics(ctx context.Context, query string) ([]metricValue, error) {
	vals := url.Values{}
	vals.Set("query", query)
	vals.Set("time", strconv.Itoa(int(time.Now().Unix())))

	// Port 8428 is hardcoded in kind.yml.
	u, err := url.Parse("http://localhost:8428")
	if err != nil {
		panic(fmt.Errorf("BUG: cannot parse URL %q: %s", u, err))
	}
	u.Path = filepath.Join(u.Path, "/api/v1/query")
	u.RawQuery = vals.Encode()

	var resp *http.Response
	for i := 0; i < 5; i++ {
		resp, err = vmClient.Get(u.String())
		if err != nil {
			slog.Warn("failed to fetch VictoriaMetrics; request will be retried", slog.String("err", err.Error()))
			if err := sleepCtx(ctx, time.Second); err != nil {
				return nil, err
			}
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code from VictoriaMetrics %s", resp.Status)
	}

	vmResponse := &struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string  `json:"metric"`
				Value  [2]json.RawMessage `json:"value"` // [timestamp, "value"]
			} `json:"result"`
		} `json:"data"`
	}{}
	if err := json.NewDecoder(resp.Body).Decode(vmResponse); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if vmResponse.Status != "success" {
		return nil, fmt.Errorf("unexpected status %q from VictoriaMetrics", vmResponse.Status)
	}

	var results []metricValue
	for _, result := range vmResponse.Data.Result {
		quoted := string(result.Value[1])
		unquoted, err := strconv.Unquote(quoted)
		if err != nil {
			panic(fmt.Errorf("BUG: cannot unquote value %q: %w", quoted, err))
		}
		val, err := strconv.ParseFloat(unquoted, 64)
		if err != nil {
			panic(fmt.Errorf("BUG: cannot parse float64 %q: %w", unquoted, err))
		}
		results = append(results, metricValue{
			labels: result.Metric,
			value:  val,
		})
	}

	return results, nil
}

type runInfo struct {
	GitHash string
}

func writeRunInfo(date time.Time) {
	gitHash := getGitHash()
	info := runInfo{
		GitHash: gitHash,
	}

	dstFolder := filepath.Join(resultsDir, date.Format(folderNameFormat))
	if err := os.MkdirAll(dstFolder, 0755); err != nil {
		fatalf("cannot create directory %q: %s", dstFolder, err)
	}
	dstFile := filepath.Join(dstFolder, "info.json")

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		panic(err)
	}

	writeFileAtomic(dstFile, data)
}

// getGitHash returns the current commit hash.
func getGitHash() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		fatalf("cannot read git commit: %s", err)
	}
	return strings.TrimSpace(string(out))
}

func loadRunInfo(date time.Time) runInfo {
	infoFile := filepath.Join(resultsDir, date.Format(folderNameFormat), "info.json")
	data, err := os.ReadFile(infoFile)
	if err != nil {
		fatalf("cannot read file %q: %s", infoFile, err)
	}

	info := runInfo{}
	if err := json.Unmarshal(data, &info); err != nil {
		fatalf("cannot parse info.json with content %q: %s", data, err)
	}
	return info
}
