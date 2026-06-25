package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	otlpcollectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeReq(spans ...*tracev1.Span) *otlpcollectortrace.ExportTraceServiceRequest {
	return &otlpcollectortrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracev1.ResourceSpans{
			{
				ScopeSpans: []*tracev1.ScopeSpans{
					{Spans: spans},
				},
			},
		},
	}
}

func TestExportBillsBillableSpanOnly(t *testing.T) {
	var received ingestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"balanceRemaining":99,"balanceQuota":0,"accepted":true}`))
	}))
	t.Cleanup(srv.Close)

	talos := &TalosIngestClient{BaseURL: srv.URL, HTTP: srv.Client()}
	pricing := PricingConfig{Default: ModelPricing{InputPerMillion: 5.0, OutputPerMillion: 15.0, CacheDiscount: 0.5}}
	s := newMeteringServer(pricing, talos, discardLogger())

	billable := billableSpan("actor-1", "gpt-4", 1_000_000, 1_000_000, 2_000_000, 0)
	// Non-billable: no actor_id.
	nonBillable := &tracev1.Span{
		TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:  []byte{30, 31, 32, 33, 34, 35, 36, 37},
		Attributes: []*commonv1.KeyValue{
			kv(attrModel, strVal("gpt-4")),
			kv(attrCompletionTokens, intVal(100)),
		},
	}

	_, err := s.Export(context.Background(), makeReq(billable, nonBillable))
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}

	snap := s.Snapshot()
	if snap.SpansSeen != 2 {
		t.Errorf("SpansSeen: got %d want 2", snap.SpansSeen)
	}
	if snap.SpansBilled != 1 {
		t.Errorf("SpansBilled: got %d want 1", snap.SpansBilled)
	}
	if snap.SpansSkipped != 1 {
		t.Errorf("SpansSkipped: got %d want 1", snap.SpansSkipped)
	}
	if snap.DebitErrors != 0 {
		t.Errorf("DebitErrors: got %d want 0", snap.DebitErrors)
	}

	// Verify the billed cost: 1M input @5 + 1M output @15 = 20.0 -> 20_000_000 micros.
	if received.CostMicros != 20_000_000 {
		t.Errorf("cost_micros: got %d want 20000000", received.CostMicros)
	}
	if received.ActorID != "actor-1" || received.Model != "gpt-4" {
		t.Errorf("ingest fields wrong: %+v", received)
	}
	if received.UsageAmount != 2_000_000 {
		t.Errorf("usage_amount: got %d want 2000000", received.UsageAmount)
	}
}

func TestExportNeverErrorsOnDebitFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"talos down"}`))
	}))
	t.Cleanup(srv.Close)

	talos := &TalosIngestClient{BaseURL: srv.URL, HTTP: srv.Client()}
	pricing := defaultPricingConfig()
	s := newMeteringServer(pricing, talos, discardLogger())

	_, err := s.Export(context.Background(), makeReq(billableSpan("a", "m", 10, 5, 15, 0)))
	if err != nil {
		t.Fatalf("Export must not propagate debit errors: %v", err)
	}
	if s.Snapshot().DebitErrors != 1 {
		t.Errorf("expected 1 debit error, got %d", s.Snapshot().DebitErrors)
	}
	if s.Snapshot().SpansBilled != 0 {
		t.Errorf("expected 0 billed, got %d", s.Snapshot().SpansBilled)
	}
}

func TestExportDedupsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"balanceRemaining":0,"balanceQuota":0,"accepted":false}`))
	}))
	t.Cleanup(srv.Close)

	talos := &TalosIngestClient{BaseURL: srv.URL, HTTP: srv.Client()}
	s := newMeteringServer(defaultPricingConfig(), talos, discardLogger())

	_, err := s.Export(context.Background(), makeReq(billableSpan("a", "m", 10, 5, 15, 0)))
	if err != nil {
		t.Fatalf("Export error: %v", err)
	}
	snap := s.Snapshot()
	if snap.Dedups != 1 {
		t.Errorf("expected 1 dedup, got %d", snap.Dedups)
	}
	// Accepted=false still counts as "handled" (billed counter tracks debit attempts that
	// reached Talos successfully), but no new ledger row was written.
	if snap.SpansBilled != 1 {
		t.Errorf("expected SpansBilled=1 (handled), got %d", snap.SpansBilled)
	}
}

func TestExportCountsRevenueLeak(t *testing.T) {
	// A span with actor_id + model but NO token attributes (the non-stream parse
	// quirk): brightstaff intended to bill it (actor stamped) but usage never
	// reached the span. Talos must NOT be called; the leak must be counted.
	talosCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		talosCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"balanceRemaining":0,"balanceQuota":0,"accepted":true}`))
	}))
	t.Cleanup(srv.Close)

	talos := &TalosIngestClient{BaseURL: srv.URL, HTTP: srv.Client()}
	s := newMeteringServer(defaultPricingConfig(), talos, discardLogger())

	leakSpan := &tracev1.Span{
		TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:  []byte{40, 41, 42, 43, 44, 45, 46, 47},
		Attributes: []*commonv1.KeyValue{
			kv(attrActorID, strVal("actor-leak")),
			kv(attrModel, strVal("gpt-4")),
		},
	}
	// A genuinely non-billing span (no actor_id) must NOT count as a leak.
	routingSpan := &tracev1.Span{
		TraceId:    []byte{2, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:     []byte{50, 51, 52, 53, 54, 55, 56, 57},
		Attributes: []*commonv1.KeyValue{kv(attrModel, strVal("gpt-4"))},
	}

	if _, err := s.Export(context.Background(), makeReq(leakSpan, routingSpan)); err != nil {
		t.Fatalf("Export error: %v", err)
	}
	snap := s.Snapshot()
	if snap.SpansSkipped != 2 {
		t.Errorf("SpansSkipped: got %d want 2", snap.SpansSkipped)
	}
	if snap.RevenueLeaks != 1 {
		t.Errorf("RevenueLeaks: got %d want 1 (only the actor-carrying span leaks)", snap.RevenueLeaks)
	}
	if snap.SpansBilled != 0 {
		t.Errorf("SpansBilled: got %d want 0", snap.SpansBilled)
	}
	if talosCalled {
		t.Error("Talos must not be called for a leaked (unbillable) span")
	}
}

func TestExportEmptyRequest(t *testing.T) {
	talos := &TalosIngestClient{BaseURL: "http://127.0.0.1:0", HTTP: &http.Client{}}
	s := newMeteringServer(defaultPricingConfig(), talos, discardLogger())
	if _, err := s.Export(context.Background(), &otlpcollectortrace.ExportTraceServiceRequest{}); err != nil {
		t.Fatalf("empty Export must not error: %v", err)
	}
}
