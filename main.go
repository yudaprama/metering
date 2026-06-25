// Command metering is the async billing consumer for the Plano→Ory auth/metering
// loop (PLANO_AUTH_TO_ORY_PLAN.md Phase 6 / M3). It receives LLM-completion
// OTLP/gRPC spans from Alloy, applies per-model pricing, and debits the actor's
// balance via the Talos fork's AdminIngestUsage endpoint. M1 (Alloy export) and
// M2 (Talos metering) are already shipped; this is the missing consumer in the
// middle that closes the async billing loop.
//
// Configuration (env):
//
//	METERING_OTLP_GRPC_ADDR  OTLP/gRPC trace receiver listen addr (default 127.0.0.1:4319)
//	METERING_HEALTH_ADDR     /healthz HTTP listen addr          (default 127.0.0.1:4320)
//	METERING_PRICING_CONFIG  path to a pricing.yaml (optional; overrides plano_config.yaml)
//	METERING_PLANO_CONFIG    path to plano_config.yaml for the billing block (default ./plano_config.yaml)
//	TALOS_URL                Talos base URL                       (default http://localhost:4420)
//	TALOS_ADMIN_TOKEN        Talos admin bearer token (optional; sent if set)
//
// The Alloy `otelcol.exporter.otlp "metering"` must point at METERING_OTLP_GRPC_ADDR
// (see config.alloy) — it MUST NOT be Alloy's own :4317 receiver (that loops).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	otlpcollectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

var version = "dev"

const shutdownTimeout = 10 * time.Second

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	otlpAddr := flag.String("otlp-addr", envOr("METERING_OTLP_GRPC_ADDR", "127.0.0.1:4319"), "OTLP/gRPC trace receiver listen address")
	healthAddr := flag.String("health-addr", envOr("METERING_HEALTH_ADDR", "127.0.0.1:4320"), "healthz HTTP listen address")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *showVersion {
		fmt.Printf("metering %s\n", version)
		os.Exit(0)
	}

	pricing := loadPricingConfig()
	log.Info("metering pricing loaded",
		"default_input_per_million", pricing.Default.InputPerMillion,
		"default_output_per_million", pricing.Default.OutputPerMillion,
		"default_cache_discount", pricing.Default.CacheDiscount,
		"model_overrides", len(pricing.Models))

	talos := NewTalosIngestClient()
	log.Info("talos ingest target", "url", talos.BaseURL+talosIngestPath, "auth", talos.AdminToken != "")

	srv := newMeteringServer(pricing, talos, log)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// OTLP/gRPC trace receiver (Alloy → metering).
	lis, err := net.Listen("tcp", *otlpAddr)
	if err != nil {
		log.Error("otlp listen failed", "addr", *otlpAddr, "error", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer()
	otlpcollectortrace.RegisterTraceServiceServer(grpcServer, srv)

	go func() {
		log.Info("OTLP/gRPC trace receiver listening", "addr", *otlpAddr)
		if err := grpcServer.Serve(lis); err != nil {
			log.Error("grpc server stopped", "error", err)
		}
	}()

	// healthz HTTP server (planoctl health check polls GET /healthz).
	hm := http.NewServeMux()
	hm.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"version": version,
			"metrics": srv.Snapshot(),
		})
	})
	healthServer := &http.Server{Addr: *healthAddr, Handler: hm}
	go func() {
		log.Info("healthz HTTP listening", "addr", *healthAddr)
		if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("health server stopped", "error", err)
		}
	}()

	log.Info("metering running", "version", version)

	<-ctx.Done()
	log.Info("shutting down metering...")
	grpcServer.GracefulStop()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutCancel()
	_ = healthServer.Shutdown(shutCtx)
	log.Info("metering stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
