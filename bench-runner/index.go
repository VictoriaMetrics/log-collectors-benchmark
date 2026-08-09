package main

import (
	"bytes"
	"cmp"
	_ "embed"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed index.gohtml
var index string

type templateData struct {
	Runs           []run
	CollectorInfos map[string]collectorInfo
	Samples        []sample
}

// buildIndexHTML reads stored reports from disk and renders HTML from them.
func buildIndexHTML() string {
	runs := loadRuns()
	infos := collectors
	samples := loadSamples()
	data := templateData{
		Runs:           runs,
		CollectorInfos: infos,
		Samples:        samples,
	}

	var bb bytes.Buffer
	tmpl := template.Must(template.New("index").Parse(index))
	if err := tmpl.Execute(&bb, data); err != nil {
		fatalf("failed to render index: %s", err)
	}
	return bb.String()
}

type run struct {
	Date            string
	Info            runInfo
	ThroughputTable throughputTable
	ResourcesTable  resourcesTable
}

// loadRuns builds one table per date folder found in resultsDir.
//
// Returned tables are sorted from the newest ones to oldest.
func loadRuns() []run {
	dates := readAvailableDates()

	var runs []run
	for _, date := range dates {
		throughputReports, staticReports, info := loadReports(date)
		if len(throughputReports) == 0 && len(staticReports) == 0 {
			fatalf("cannot find reports for date %s", date)
		}

		r := run{
			Date:            date.Format("02 Jan 2006"),
			Info:            info,
			ThroughputTable: buildThroughputTable(throughputReports),
			ResourcesTable:  buildResourcesTable(staticReports),
		}
		runs = append(runs, r)
	}

	slices.SortFunc(runs, func(a, b run) int {
		return -cmp.Compare(a.Date, b.Date)
	})
	return runs
}

// loadReports returns benchmark results for two different cases:
//   - The first case 'throughput' measures maximum throughput for each log collector.
//   - The second case 'resources' measures resources usage with static load for each log collector.
func loadReports(date time.Time) (maxReports, staticReports []collectorReport, info runInfo) {
	for collector := range collectors {
		rep, ok := tryReadReportForDate(date, collector, "throughput")
		if ok {
			maxReports = append(maxReports, rep)
		}
		rep, ok = tryReadReportForDate(date, collector, "resources")
		if ok {
			// Override status if collector lost too many rows under the static load.
			//
			// Vector and Fluent Bit are known to lose up to 100 logs even under normal load, see these issues:
			// https://github.com/fluent/fluent-bit/issues/11602
			// https://github.com/vectordotdev/vector/issues/24981
			if rep.Success && rep.Lost > 100 {
				rep.Success = false
				rep.FailReason = "lost too many rows"
			}
			staticReports = append(staticReports, rep)
		}
	}
	info = loadRunInfo(date)
	return maxReports, staticReports, info
}

type throughputTable struct {
	Rows   []maxThroughputRow
	Failed int
	Passed int
}

type maxThroughputRow struct {
	Collector     string
	Version       string
	Status        string
	MaxThroughput string
	// ThroughputPercent in range [0, 100], relative to the fastest log-collector.
	ThroughputPercent float64
}

func buildThroughputTable(maxReports []collectorReport) throughputTable {
	sortReportsByThroughput(maxReports)

	var maxThroughput int
	if len(maxReports) > 0 {
		maxThroughput = maxReports[0].Throughput
	}
	t := throughputTable{}
	for _, rep := range maxReports {
		if rep.Success {
			t.Passed++
		} else {
			t.Failed++
		}

		t.Rows = append(t.Rows, maxThroughputRow{
			Collector:         rep.Collector,
			Version:           rep.Version,
			Status:            rowStatus(rep),
			MaxThroughput:     itoa(rep.Throughput),
			ThroughputPercent: percent(float64(rep.Throughput), float64(maxThroughput)),
		})
	}

	return t
}

// percent returns the percentage of v relative to highest in range [0.0, 100.0].
func percent(v, highest float64) float64 {
	if highest == 0 {
		return 0
	}
	p := v / highest * 100
	if p > 100 {
		p = 100
	}
	if p < 0 {
		p = 0
	}
	return p
}

type resourcesTable struct {
	Rows   []resourcesRow
	Failed int
	Passed int
}

type resourcesRow struct {
	Collector string
	Version   string
	Status    string

	AvgCPUUsage        string
	AvgCPUUsagePercent float64

	MaxMemoryMiB        string
	MaxMemoryMiBPercent float64

	AvgMemoryMiB        string
	AvgMemoryMiBPercent float64

	NetworkBytesPerLog        string
	NetworkBytesPerLogPercent float64

	Lost string
}

func buildResourcesTable(staticReports []collectorReport) resourcesTable {
	sortReportsByCPU(staticReports)

	var highestCPU, highestAvgMem, highestMaxMem, highestNet float64
	for _, rep := range staticReports {
		highestCPU = max(highestCPU, rep.AvgCPUUsage)
		highestAvgMem = max(highestAvgMem, float64(rep.AvgMemory))
		highestMaxMem = max(highestMaxMem, float64(rep.MaxMemory))
		highestNet = max(highestNet, float64(rep.NetworkBytesPerLog))
	}

	var t resourcesTable
	for _, rep := range staticReports {
		if rep.Success {
			t.Passed++
		} else {
			t.Failed++
		}

		t.Rows = append(t.Rows, resourcesRow{
			Collector: rep.Collector,
			Version:   rep.Version,
			Status:    rowStatus(rep),

			AvgCPUUsage:        fmt.Sprintf("%.2f", rep.AvgCPUUsage),
			AvgCPUUsagePercent: percent(rep.AvgCPUUsage, highestCPU),

			MaxMemoryMiB:        mib(rep.MaxMemory),
			MaxMemoryMiBPercent: percent(float64(rep.MaxMemory), highestMaxMem),

			AvgMemoryMiB:        mib(rep.AvgMemory),
			AvgMemoryMiBPercent: percent(float64(rep.AvgMemory), highestAvgMem),

			NetworkBytesPerLog:        itoa(rep.NetworkBytesPerLog),
			NetworkBytesPerLogPercent: percent(float64(rep.NetworkBytesPerLog), highestNet),

			Lost: itoaShort(rep.Lost),
		})
	}

	return t
}

func mib(b int) string {
	return fmt.Sprintf("%.1f", float64(b)/1024/1024)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func itoaShort(n int) string {
	if n < 1000 {
		return itoa(n)
	}
	a := n / 1000
	b := (n % 1000) / 100
	return fmt.Sprintf("%d.%dk", a, b)
}

type sample struct {
	Collector string
	Data      string
}

// loadSamples reads log samples for each log collector.
//
// Special case is a fake collector called "original",
// which represents original unprocessed log message produced by log-generator.
func loadSamples() []sample {
	dir := filepath.Join(resultsDir, "samples")
	des, err := os.ReadDir(dir)
	if err != nil {
		fatalf("failed to read samples directory %q: %s", dir, err)
	}

	var samples []sample
	for _, de := range des {
		name, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok {
			continue
		}

		path := filepath.Join(dir, de.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			fatalf("failed to read sample %q: %s", path, err)
		}
		samples = append(samples, sample{Collector: name, Data: string(content)})
	}

	slices.SortFunc(samples, func(a, b sample) int {
		// Handle "original" as special case - it must be moved to the first position.
		if a.Collector == "original" {
			return -1
		}
		if b.Collector == "original" {
			return 1
		}
		return cmp.Compare(a.Collector, b.Collector)
	})
	return samples
}

func sortReportsByThroughput(rows []collectorReport) {
	slices.SortFunc(rows, func(a, b collectorReport) int {
		if a.Success != b.Success {
			if a.Success {
				return -1
			}
			return 1
		}
		if a.Throughput == b.Throughput {
			// Throughput is the same, sort by name asc.
			return cmp.Compare(a.Collector, b.Collector)
		}
		// Sort by throughput desc.
		return -cmp.Compare(a.Throughput, b.Throughput)
	})
}

func sortReportsByCPU(rows []collectorReport) {
	slices.SortFunc(rows, func(a, b collectorReport) int {
		if a.Success != b.Success {
			if a.Success {
				return -1
			}
			return 1
		}
		if a.AvgCPUUsage == b.AvgCPUUsage {
			// CPU is the same, sort by name asc.
			return cmp.Compare(a.Collector, b.Collector)
		}
		// Sort by CPU asc.
		return cmp.Compare(a.AvgCPUUsage, b.AvgCPUUsage)
	})
}

func rowStatus(rep collectorReport) string {
	if rep.Success {
		return "pass"
	}

	if rep.FailReason == "" {
		fatalf("fail reason cannot be empty")
	}
	return "fail: " + rep.FailReason
}
