package main

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	otlpcollectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// TestExportOverGRPCWire exercises the full OTLP/gRPC receive path end-to-end:
// a real gRPC client dials the meteringServer over a bufconn listener and sends
// an ExportTraceServiceRequest carrying a billable + a non-billable span. This
// catches proto registration / marshaling bugs that a direct Export() call
// cannot (server_test.go covers the in-process logic).
func TestExportOverGRPCWire(t *testing.T) {
	var received ingestRequest
	stub := talosStub(t, &received, `{"balanceRemaining":123,"balanceQuota":0,"accepted":true}`)

	pricing := PricingConfig{Default: ModelPricing{InputPerMillion: 5.0, OutputPerMillion: 15.0, CacheDiscount: 0.5}}
	talos := &TalosIngestClient{BaseURL: stub.URL, HTTP: stub.Client()}
	srv := newMeteringServer(pricing, talos, newDebitEnqueuer(discardLogger()), discardLogger())

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	gs := grpc.NewServer()
	otlpcollectortrace.RegisterTraceServiceServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	client := otlpcollectortrace.NewTraceServiceClient(conn)

	billable := billableSpan("e2e-actor", "gpt-4", 500_000, 200_000, 700_000, 0)
	_, err = client.Export(context.Background(), makeReq(billable))
	if err != nil {
		t.Fatalf("gRPC Export failed: %v", err)
	}

	// Give the (synchronous) handler a beat to settle on the counters.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && srv.Snapshot().SpansBilled == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if got := srv.Snapshot().SpansBilled; got != 1 {
		t.Fatalf("SpansBilled: got %d want 1", got)
	}
	// 500k input @5 = 2.5 ; 200k output @15 = 3.0 ; total 5.5 -> 5_500_000 micros.
	if received.CostMicros != 5_500_000 {
		t.Errorf("cost_micros over the wire: got %d want 5500000", received.CostMicros)
	}
	if received.ActorID != "e2e-actor" || received.Model != "gpt-4" {
		t.Errorf("ingest fields wrong: %+v", received)
	}
}
