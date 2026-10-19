package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1/collectorv1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

var errMissingID = errors.New("collector id is required")

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	var level slog.Level
	_ = level.UnmarshalText([]byte(env("LOG_LEVEL", "info")))
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if strings.EqualFold(env("LOG_FORMAT", "text"), "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	log := slog.New(h)

	dir := env("CONFIG_DIR", "/configs")
	addr := env("LISTEN_ADDR", ":8080")
	logIdle := !strings.EqualFold(env("LOG_NOT_MODIFIED", "true"), "false")

	store := NewStore(dir, time.Second, log)
	srv := NewServer(store, log, logIdle)

	mux := http.NewServeMux()
	path, handler := collectorv1connect.NewCollectorServiceHandler(srv, connect.WithCompressMinBytes(1024))
	mux.Handle(path, handler)
	srv.registerUI(mux)

	httpSrv := &http.Server{
		Addr: addr,
		// h2c: на одном порту работают и Connect поверх HTTP/1.1, и gRPC поверх HTTP/2 без TLS.
		Handler:           h2c.NewHandler(mux, &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	cfgs := store.Configs()
	log.Info("started", "listen", addr, "config_dir", dir, "pipelines", len(cfgs))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
}
