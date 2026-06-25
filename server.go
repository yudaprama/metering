package main

import (
	"context"
	"encoding/hex"
	"log/slog"
	"sync/atomic"

	otlpcollectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

// meteringServer implements otlpcollectortrace.TraceServiceServer. It receives
// OTLP/gRPC trace exports from Alloy, extracts billable LLM-completion spans,
// applies pricing, and debits the actor's balance via the Talos fork.
type meteringServer struct {
	otlpcollectortrace.UnimplementedTraceServiceServer

	pricing PricingConfig
	talos   *TalosIngestClient
	log     *slog.Logger

	spansSeen    atomic.Int64
	spansBilled  atomic.Int64
	spansSkipped atomic.Int64
	debitErrors  atomic.Int64
	dedups       atomic.Int64
	revenueLeaks atomic.Int64 // skipped spans that carried an actor_id (escaped billing)
}

func newMeteringServer(pricing PricingConfig, talos *TalosIngestClient, log *slog.Logger) *meteringServer {
	return &meteringServer{pricing: pricing, talos: talos, log: log}
}

// Export is the OTLP/gRPC trace export RPC. Alloy calls it with batches of
// spans. We never return an error: a non-nil error makes Alloy retry the whole
// batch (risking duplicate debits for spans that already succeeded). Per-span
// ingest failures are logged and counted, not propagated.
func (s *meteringServer) Export(ctx context.Context, req *otlpcollectortrace.ExportTraceServiceRequest) (*otlpcollectortrace.ExportTraceServiceResponse, error) {
	for _, rs := range req.GetResourceSpans() {
		if rs == nil {
			continue
		}
		for _, ss := range rs.GetScopeSpans() {
			if ss == nil {
				continue
			}
			for _, span := range ss.GetSpans() {
				s.handleSpan(ctx, span)
			}
		}
	}
	return &otlpcollectortrace.ExportTraceServiceResponse{}, nil
}

func (s *meteringServer) handleSpan(ctx context.Context, span *tracev1.Span) {
	if span == nil {
		return
	}
	s.spansSeen.Add(1)

	ev, ok := extractEvent(span)
	if !ok {
		s.spansSkipped.Add(1)
		// A skipped span that still carries billing.actor_id is a billable LLM
		// call that escaped metering — the provider was paid but the actor's
		// quota is not debited (revenue leak + gate bypass). Surface it loudly so
		// it can be alerted on (Alloy ships these WARN logs to Loki/Grafana),
		// rather than letting it disappear into the generic skip counter.
		if leak, isLeak := spanLeak(span); isLeak {
			s.revenueLeaks.Add(1)
			s.log.Warn("billable span could not be metered (revenue leak)",
				"reason", leak.Reason,
				"actor_id", leak.ActorID, "model", leak.Model,
				"trace_id", hex.EncodeToString(span.GetTraceId()),
				"span_id", hex.EncodeToString(span.GetSpanId()))
		}
		return
	}

	pricing := s.pricing.PricingFor(ev.Model)
	costMicros := pricing.CostMicros(ev.Usage)
	usageAmount := ev.Usage.PromptTokens + ev.Usage.CompletionTokens

	res, err := s.talos.Ingest(ctx, ingestRequest{
		ActorID:     ev.ActorID,
		UsageType:   usageTypeTokens,
		UsageAmount: usageAmount,
		CostMicros:  costMicros,
		Model:       ev.Model,
		RequestID:   ev.RequestID,
	})
	if err != nil {
		s.debitErrors.Add(1)
		s.log.Error("debit failed",
			"actor_id", ev.ActorID, "model", ev.Model,
			"trace_id", ev.TraceID, "span_id", ev.SpanID,
			"cost_micros", costMicros, "error", err)
		return
	}
	s.spansBilled.Add(1)
	if res.Duplicate {
		s.dedups.Add(1)
	}
	s.log.Info("billed usage",
		"actor_id", ev.ActorID, "model", ev.Model,
		"prompt", ev.Usage.PromptTokens, "completion", ev.Usage.CompletionTokens,
		"cached", ev.Usage.CachedInputTokens,
		"usage_amount", usageAmount, "cost_micros", costMicros,
		"balance_remaining", res.BalanceRemaining, "balance_quota", res.BalanceQuota,
		"duplicate", res.Duplicate,
		"trace_id", ev.TraceID, "span_id", ev.SpanID)
}

// MetricsSnapshot is a point-in-time copy of the counters, surfaced on the
// /healthz endpoint for observability.
type MetricsSnapshot struct {
	SpansSeen    int64 `json:"spans_seen"`
	SpansBilled  int64 `json:"spans_billed"`
	SpansSkipped int64 `json:"spans_skipped"`
	DebitErrors  int64 `json:"debit_errors"`
	Dedups       int64 `json:"dedups"`
	// RevenueLeaks counts skipped spans that carried a billing.actor_id — LLM
	// calls that escaped metering (provider paid, quota not debited). A non-zero
	// value here means the balance gate is being bypassed; alert on its rate.
	RevenueLeaks int64 `json:"revenue_leaks"`
}

func (s *meteringServer) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		SpansSeen:    s.spansSeen.Load(),
		SpansBilled:  s.spansBilled.Load(),
		SpansSkipped: s.spansSkipped.Load(),
		DebitErrors:  s.debitErrors.Load(),
		Dedups:       s.dedups.Load(),
		RevenueLeaks: s.revenueLeaks.Load(),
	}
}
