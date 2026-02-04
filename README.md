# Log Collectors Benchmark

A benchmark suite for comparing log collectors shipping to VictoriaLogs,
with log delivery verification.

Beyond performance numbers, this tool continuously verifies that every log record
is delivered exactly once - no losses, no duplicates. 
At VictoriaMetrics, we use it to ensure vlagent reliably delivers logs under any load.

## Overview

Tested collectors:

- [VictoriaLogs Agent](https://docs.victoriametrics.com/victorialogs/vlagent/) (vlagent) v1.47.0
- Vector v0.53.0
- Promtail v3.5.1
- Grafana Alloy v1.13.2
- Grafana Agent v0.44.2
- Fluent Bit v4.2.3
- OpenTelemetry Collector v0.146.1
- Filebeat v9.3.1
- Fluentd v1.19.1

Runs in a local k8s cluster (using `kind`).

What is measured:

- CPU and memory usage per collector.
- Throughput.
- Missing logs.
- Duplicated logs.

## Results

### Maximum throughput

Tested on Google Cloud `n2-highcpu-32` (32 vCPUs, 32 GiB Memory), single `kind` node.
Each collector has 1 CPU and 1 GiB memory limit.

| Collector               | Throughput (logs/sec) | Best scenario |
|-------------------------|-----------------------|---------------|
| vlagent                 | 169 230               | 50 Pods       |
| Fluent Bit              | 41 179                | 10 Pods       |
| Vector                  | 33 328                | 1 Pod         |
| Grafana Agent           | 29 310                | 1 Pod         |
| promtail                | 26 733                | 1 Pod         |
| OpenTelemetry Collector | 24 003                | 1 Pod         |
| Alloy                   | 21 082                | 1 Pod         |
| fluentd                 | 8 980                 | 1 Pod         |
| Filebeat                | 4 222                 | 200 Pods      |

### Detailed results

Load was increased linearly throughout the test,
starting from a low rate and reaching the 180k logs/sec over ~1 hour.

> **Note**: (*) 1 log-generator Pod cannot produce more than ~156k logs/sec because `containerd` becomes the bottleneck at high throughput.
> Results for the 1 Pod scenario are therefore capped at this limit.

| Pods  | Snapshot                                                                                  |
|-------|-------------------------------------------------------------------------------------------|
| 1 (*) | [view](https://snapshots.raintank.io/dashboard/snapshot/k24ug8P9sViCX6IA6EyEev6eOdvTivdL) |
| 10    | [view](https://snapshots.raintank.io/dashboard/snapshot/nCy8El4VdDPmVHGlnr1VBj5IqImO86x8) |
| 25    | [view](https://snapshots.raintank.io/dashboard/snapshot/fRlalVKgWPkd0T5hRquH4hK6QWBnf5Pn) |
| 50    | [view](https://snapshots.raintank.io/dashboard/snapshot/xkUJikjTiNhKmTmQbSYLOzhNJx9jS0fn) |
| 100   | [view](https://snapshots.raintank.io/dashboard/snapshot/kdDAHmDetCskfBRSQTvmOZDzDdb38uC5) |
| 150   | [view](https://snapshots.raintank.io/dashboard/snapshot/KGMc0OEQnsDCxxcYme2nYVljZp76dx2B) |
| 200   | [view](https://snapshots.raintank.io/dashboard/snapshot/aJeEmSXeynQbFifrlXwrdNvONJo4yXsk) |

We do not measure results for more than 200 Pods, since it is [recommended to keep 110 Pods per node](https://kubernetes.io/docs/setup/best-practices/cluster-large/).

## How does it work?

1. log-generator - generates JSON logs.
2. Log collector - tail logs from log-generator Pods, ship to VictoriaLogs.
3. VictoriaLogs - stores all logs.
4. log-verifier - checks VictoriaLogs for missing/duplicate logs, exposes metrics.
5. VictoriaMetrics - stores metrics; vmagent - collects CPU/RAM metrics of containers and metrics from log-verifier.
6. Grafana - displays metrics and resource usage.

Simplified diagram:

<img src="how-does-it-work.svg" alt="how-does-it-work">

## Prerequisites

- Docker
- kubectl
- [`kind`](https://kind.sigs.k8s.io/)
- helm
- make

## Quick Start

Test all collectors:

```sh
make bench-up-all
```

What it does:

1. Creates kind cluster.
2. Builds log-generator and log-verifier images.
3. Deploys VictoriaLogs Single.
4. Deploys VictoriaMetrics + Grafana.
5. Deploys all collectors.
6. Starts log-generator Pods.
7. Runs log-verifier.
8. Follow [Monitoring](#monitoring) instructions to see results in Grafana.

**Recommended**: Run collectors individually to avoid disk I/O bottlenecks on the host machine.
See [Test individual collector](#test-individual-collector).

## Test individual collector

Prepare `kind` cluster, deploy VictoriaMetrics, VictoriaLogs, log-generator, log-verifier, and Grafana:

```sh
make bench-prepare           
```

Deploy collector:

```sh
make bench-up-vlagent        # VictoriaLogs Agent
make bench-up-vector         # Vector
make bench-up-promtail       # Promtail
make bench-up-alloy          # Grafana Alloy
make bench-up-grafana-agent  # Grafana Agent
make bench-up-fluent-bit     # Fluent Bit
make bench-up-otel-collector # OpenTelemetry Collector
make bench-up-filebeat       # Filebeat
make bench-up-fluentd        # Fluentd
```

Run load generator:

```sh
make bench-up-generator
```

## Configuration

### Log Generator

Generates JSON logs with:

- Timestamp (nanosecond precision).
- Sequence ID (for verification).
- Log level (DEBUG/INFO/WARN/ERROR).
- Component name.
- HTTP method, status code.
- Duration, user ID, bytes.
- Environment, region, trace ID.

Change the log generation rate via `-logsPerSecond` flag in `log-generator/deployment.yml`.

To gradually increase the load over time, use ramp-up flags:

- `-rampUp` - enable gradual load increase.
- `-rampUp.step` - logs per second to add at each step.
- `-rampUp.interval` - how often to increase the load (minimum 1 second).

Example: start at 100 logs/sec and increase by 500 every 2 minutes:

```sh
./log-generator -logsPerSecond=100 -rampUp=true -rampUp.step=500 -rampUp.interval=2m
```

### Collectors

All collectors read logs from `/var/log/pods` or `/var/log/containers`, parse content where possible, and ship to
VictoriaLogs at:
`http://vls-victoria-logs-single-server.monitoring.svc.cluster.local:9428/insert/<protocol>`

Collectors are configured to compress request bodies to reduce network traffic
and better emulate production environments.
Different collectors use different compression algorithms (gzip, snappy, zstd)
and protocols (JSON, protobuf), which can impact performance.

Configurations are based on official Helm chart defaults with
minimal modifications required for the benchmark environment.
No heavy tuning is applied to ensure fair comparison, including:

- No custom buffer sizes or queue depths.
- No custom batch sizes or flush intervals.
- No additional worker threads or parallelism settings.
- No runtime tuning such as garbage collection settings.

All collectors have identical resource requests and limits (1 CPU, 1 GiB memory) for fair comparison
and reduce the chance of CPU/RAM contention when running multiple collectors simultaneously.

## Monitoring

A Grafana dashboard is provisioned automatically during setup.
It visualizes collector performance, resource usage, and log delivery quality.

Access dashboard:

```sh
kubectl port-forward -n monitoring svc/vms-grafana 3000:80
```

Navigate to http://localhost:3000 (credentials: `admin`/`admin`).

### Query VictoriaLogs

Access VictoriaLogs Web UI:

```sh
kubectl port-forward -n monitoring svc/vls-victoria-logs-single-server 9428:9428
```

Navigate to http://localhost:9428 or query via API:

```sh
curl -X POST 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=_time:5m collector:="vlagent"'
```

## Verification

log-verifier validates log delivery by tracking sequence IDs in logs generated by log-generator.
Each log contains a unique, monotonically increasing sequence ID (field `sequence_id`).

The verification process runs every 20 seconds and queries VictoriaLogs for logs from a 1-minute time window that
occurred 1 minute in the past.
This 1-minute delay accounts for collectors that may deliver logs with latency.

The verifier discovers active collectors and log-generator Pods, fetches logs from VictoriaLogs for the delayed time
window, and analyzes each collector-pod pair.
It extracts sequence IDs, verifies monotonic ordering, detects gaps indicating missed logs, and identifies duplicate
sequence IDs.
All findings are exposed as Prometheus metrics for monitoring (see [Monitoring](#monitoring)).

## Cleanup

```sh
make bench-down
```

## Notes

- Since it's challenging to configure all collectors to use identical log formats,
  each collector may produce different output formats.
