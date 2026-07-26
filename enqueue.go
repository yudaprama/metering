package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// debitEnqueuer forwards failed Talos debits to the egent-jobs trigger
// endpoint (POST {EGENT_JOBS_URL}/trigger/debit-retry) so they get durable
// retry instead of becoming permanent revenue leaks. It is fire-and-forget:
// metering's OTLP export hot path must never block or fail because the retry
// queue is unreachable.
type debitEnqueuer struct {
	url     string
	hc      *http.Client
	log     *slog.Logger
	enabled bool
}

func newDebitEnqueuer(log *slog.Logger) *debitEnqueuer {
	base := strings.TrimRight(os.Getenv("EGENT_JOBS_URL"), "/")
	if base == "" {
		log.Info("debit retry enqueuer disabled (set EGENT_JOBS_URL to enable)")
		return &debitEnqueuer{log: log}
	}
	return &debitEnqueuer{
		url:     base + "/trigger/debit-retry",
		hc:      &http.Client{Timeout: 3 * time.Second},
		log:     log,
		enabled: true,
	}
}

// enqueueDebit hands a failed debit to egent-jobs for durable retry. The
// ingest payload already uses the camelCase JSON shape egent-jobs' debitretry
// Args expects (actorId/requestId/...). Best-effort: errors are logged,
// never propagated.
func (e *debitEnqueuer) enqueueDebit(req ingestRequest) {
	if !e.enabled {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		body, _ := json.Marshal(req)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
		if err != nil {
			e.log.Warn("debit enqueue: build request", "requestId", req.RequestID, "error", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := e.hc.Do(httpReq)
		if err != nil {
			e.log.Warn("debit enqueue: post failed (egent-jobs unreachable?)", "requestId", req.RequestID, "error", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			e.log.Warn("debit enqueue: non-202 from egent-jobs", "status", resp.StatusCode, "requestId", req.RequestID)
			return
		}
		e.log.Info("debit enqueued for durable retry", "requestId", req.RequestID, "actorId", req.ActorID)
	}()
}
