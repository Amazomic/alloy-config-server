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

	addr := env("LISTEN_ADDR", ":8080")
	mux := http.NewServeMux()
	mode := strings.ToLower(env("APP_MODE", "server"))
	var service collectorv1connect.CollectorServiceHandler
	switch mode {
	case "server":
		store := NewStore(env("CONFIG_DIR", "/configs"), time.Second, log)
		srv := NewServer(store, log, !strings.EqualFold(env("LOG_NOT_MODIFIED", "true"), "false"))
		srv.registerUI(mux)
		service = srv
		log.Info("configs loaded", "configs", len(store.Configs()))
	case "relay":
		timeout, timeoutErr := time.ParseDuration(env("RELAY_TIMEOUT", "5s"))
		maxAge, ageErr := time.ParseDuration(env("RELAY_CACHE_MAX_AGE", "0s"))
		if timeoutErr != nil || ageErr != nil {
			log.Error("invalid relay duration settings")
			os.Exit(1)
		}
		relay, err := NewRelay(env("UPSTREAM_URL", ""), env("RELAY_CACHE_DIR", "/var/lib/alloy-config-server/relay-cache"), timeout, maxAge, env("UPSTREAM_BEARER_TOKEN", ""), log)
		if err != nil {
			log.Error("cannot start relay", "err", err)
			os.Exit(1)
		}
		relay.registerUI(mux)
		service = relay
	default:
		log.Error("APP_MODE must be server or relay")
		os.Exit(1)
	}
	path, handler := collectorv1connect.NewCollectorServiceHandler(service,
		connect.WithCompressMinBytes(1024),
		connect.WithReadMaxBytes(16<<20),
	)
	mux.Handle(path, handler)

	httpSrv := &http.Server{
		Addr: addr,
		// h2c: на одном порту работают и Connect поверх HTTP/1.1, и gRPC поверх HTTP/2 без TLS.
		Handler:           h2c.NewHandler(mux, &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	log.Info("Alloy Configuration Hub started", "listen", addr, "mode", mode)

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
