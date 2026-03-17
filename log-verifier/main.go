package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/VictoriaMetrics/VictoriaLogs/app/vlinsert"
	"github.com/VictoriaMetrics/VictoriaLogs/app/vlinsert/insertutil"
	"github.com/VictoriaMetrics/metrics"
)

var listenAddr = flag.String("listenAddr", ":8080", "")

func main() {
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	insertutil.SetLogRowsStorage(storage)

	go mustStartServer(*listenAddr)

	<-ctx.Done()
	slog.Info("received termination signal; exiting")
}

func mustStartServer(listenAddr string) {
	handle := func(w http.ResponseWriter, r *http.Request) {
		if vlinsert.RequestHandler(w, r) {
			return
		}
		if r.URL.Path == "/metrics" {
			metrics.WritePrometheus(w, false)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/debug/pprof") {
			pprof.Index(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}

	slog.Info("starting debug server", "addr", listenAddr)
	if err := http.ListenAndServe(listenAddr, http.HandlerFunc(handle)); err != nil {
		panic(err)
	}
}
