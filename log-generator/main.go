package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

var (
	logsPerSecond = flag.Int("logsPerSecond", 100, "initial number of logs to generate per second")

	rampUp         = flag.Bool("rampUp", false, "gradually increase logs per second")
	rampUpStep     = flag.Int("rampUp.step", 100, "logs per second to add each step")
	rampUpInterval = flag.Duration("rampUp.interval", 10*time.Second, "how long to run each step, minimum 1 second")
)

func main() {
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}

	time.Local = time.UTC

	if *rampUpInterval < time.Second {
		*rampUpInterval = time.Second
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	rampUpIntervalSecs := uint64(*rampUpInterval / time.Second)

	lps := *logsPerSecond
	if lps <= 0 {
		panic(fmt.Errorf("logs per second must be positive"))
	}

	iters := uint64(0)
	for {
		iters++
		logInterval := time.Second / time.Duration(lps)
		current := time.Now()
		generateLogs(lps, current, logInterval)

		pause := time.Second - time.Since(current)
		if pause <= 0 {
			panic(fmt.Errorf("cannot generate %d logs per second", lps))
		}
		ticker.Reset(pause)
		<-ticker.C

		// Each iteration takes 1 second,
		// so we increase logs per second by *rampUpStep every rampUpIntervalSecs iterations.
		if *rampUp && iters%rampUpIntervalSecs == 0 {
			lps += *rampUpStep
		}
	}
}

func generateLogs(n int, current time.Time, interval time.Duration) {
	for i := n - 1; i >= 0; i-- {
		// Write logs for the previous second.
		logTime := current.Add(-time.Duration(i) * interval)
		generateLog(logTime)
	}
	flush()
}

var id = uint64(1)
var rng = NewRNG()

var logLevels = []string{"DEBUG", "INFO", "WARN", "ERROR"}
var components = []string{"auth-service", "payment-service", "user-service", "notification-service"}
var methods = []string{"GET", "POST", "PUT", "DELETE"}
var statusCodes = []int{200, 201, 400, 401, 403, 404, 500, 503}
var messages = []string{
	"Request processed successfully",
	"User authentication completed",
	"Payment transaction initiated",
	"Database connection established",
	"Cache miss occurred",
	"Service health check passed",
	"API rate limit exceeded",
	"Session expired",
	"Data validation failed",
}
var environments = []string{"production", "staging", "development", "testing"}
var regions = []string{"us-east-1", "us-west-2", "eu-central-1", "ap-southeast-1"}
var errorTypes = []string{"TimeoutError", "ConnectionError", "ValidationError", "AuthenticationError", "DatabaseError"}
var traceIDs = []string{"trace-a1b2c3d4", "trace-e5f6g7h8", "trace-i9j0k1l2", "trace-m3n4o5p6"}
var protocols = []string{"HTTP/1.1", "HTTP/2", "gRPC", "WebSocket"}

func generateLog(current time.Time) {
	buf = append(buf, `{"generated_at":`...)
	buf = strconv.AppendInt(buf, current.UnixNano(), 10)

	buf = append(buf, `,"_msg":"`...)
	buf = append(buf, messages[rng.IntN(len(messages))]...)

	buf = append(buf, `","sequence_id":`...)
	buf = strconv.AppendUint(buf, id, 10)
	id++

	if rng.IntN(100) < 80 {
		buf = append(buf, `,"level":"`...)
		buf = append(buf, logLevels[rng.IntN(len(logLevels))]...)
		buf = append(buf, '"')
	}

	if rng.IntN(100) < 90 {
		buf = append(buf, `,"component":"`...)
		buf = append(buf, components[rng.IntN(len(components))]...)
		buf = append(buf, '"')
	}

	if rng.IntN(100) < 70 {
		buf = append(buf, `,"method":"`...)
		buf = append(buf, methods[rng.IntN(len(methods))]...)
		buf = append(buf, '"')
	}

	if rng.IntN(100) < 85 {
		buf = append(buf, `,"status_code":`...)
		buf = strconv.AppendInt(buf, int64(statusCodes[rng.IntN(len(statusCodes))]), 10)
	}

	if rng.IntN(100) < 75 {
		buf = append(buf, `,"duration_ms":`...)
		buf = strconv.AppendInt(buf, int64(rng.IntN(1000)+10), 10)
	}

	if rng.IntN(100) < 60 {
		buf = append(buf, `,"user_id":"user_`...)
		buf = strconv.AppendInt(buf, int64(rng.IntN(10000)), 10)
		buf = append(buf, '"')
	}

	if rng.IntN(100) < 65 {
		buf = append(buf, `,"bytes_sent":`...)
		buf = strconv.AppendInt(buf, int64(rng.IntN(10000)+100), 10)
	}

	if rng.IntN(100) < 15 {
		buf = append(buf, `,"environment":"`...)
		buf = append(buf, environments[rng.IntN(len(environments))]...)
		buf = append(buf, '"')
	}

	if rng.IntN(100) < 20 {
		buf = append(buf, `,"region":"`...)
		buf = append(buf, regions[rng.IntN(len(regions))]...)
		buf = append(buf, '"')
	}

	if rng.IntN(100) < 10 {
		buf = append(buf, `,"error_type":"`...)
		buf = append(buf, errorTypes[rng.IntN(len(errorTypes))]...)
		buf = append(buf, '"')
	}

	if rng.IntN(100) < 25 {
		buf = append(buf, `,"trace_id":"`...)
		buf = append(buf, traceIDs[rng.IntN(len(traceIDs))]...)
		buf = append(buf, '"')
	}

	if rng.IntN(100) < 18 {
		buf = append(buf, `,"protocol":"`...)
		buf = append(buf, protocols[rng.IntN(len(protocols))]...)
		buf = append(buf, '"')
	}

	buf = append(buf, '}')
	buf = append(buf, '\n')

	if cap(buf)-1024 < len(buf) {
		flush()
	}
}

var buf = make([]byte, 0, 16*1024)

func flush() {
	_, err := os.Stdout.Write(buf)
	if err != nil {
		panic(err)
	}
	buf = buf[:0]
}

// RNG is a fast pseudo-random number generator.
// It is based on https://github.com/valyala/fastrand and is used to reduce CPU overhead
// of random number generation when producing high-throughput log output.
type RNG struct {
	x uint32
}

func NewRNG() *RNG {
	return &RNG{
		x: getRandomUint32(),
	}
}

func (r *RNG) Uint32() uint32 {
	x := r.x
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	r.x = x
	return x
}

func (r *RNG) IntN(maxN int) uint32 {
	u32 := uint32(maxN)
	x := r.Uint32()
	return uint32((uint64(x) * uint64(u32)) >> 32)
}

func getRandomUint32() uint32 {
	x := time.Now().UnixNano()
	return uint32((x >> 32) ^ x)
}
