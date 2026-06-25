package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	talosIngestPath = "/v2alpha1/admin/usage:ingest"
	usageTypeTokens = "tokens"
)

// TalosIngestClient calls the Talos fork's AdminIngestUsage HTTP endpoint
// (POST /v2alpha1/admin/usage:ingest). Talos atomically debits the actor's
// balance and appends an api_key_usage ledger row (idempotent on request_id).
//
// Talos OSS does not bundle an identity provider for the admin surface, so the
// admin routes rely on network isolation (localhost). The admin token is sent
// when configured (TALOS_ADMIN_TOKEN) so the client works unchanged if/when a
// token check is added; it is a harmless header otherwise.
type TalosIngestClient struct {
	BaseURL    string
	AdminToken string
	HTTP       *http.Client
}

func NewTalosIngestClient() *TalosIngestClient {
	base := strings.TrimRight(os.Getenv("TALOS_URL"), "/")
	if base == "" {
		base = "http://localhost:4420"
	}
	return &TalosIngestClient{
		BaseURL:    base,
		AdminToken: os.Getenv("TALOS_ADMIN_TOKEN"),
		HTTP:       &http.Client{Timeout: 10 * time.Second},
	}
}

// ingestRequest is the protojson (camelCase) body for AdminIngestUsage. The
// gRPC-gateway JSON marshaler emits/accepts lowerCamelCase field names.
type ingestRequest struct {
	ActorID     string `json:"actorId"`
	KeyID       string `json:"keyId,omitempty"`
	UsageType   string `json:"usageType"`
	UsageAmount int64  `json:"usageAmount"`
	CostMicros  int64  `json:"costMicros"`
	Model       string `json:"model"`
	RequestID   string `json:"requestId"`
}

// ingestResponse is the protojson (camelCase) body for IngestUsageResponse.
type ingestResponse struct {
	BalanceRemaining int64 `json:"balanceRemaining"`
	BalanceQuota     int64 `json:"balanceQuota"`
	Accepted         bool  `json:"accepted"`
}

// IngestResult reports the outcome of a debit.
type IngestResult struct {
	Accepted         bool
	BalanceRemaining int64
	BalanceQuota     int64
	// Duplicate is true when Talos reports a replayed request_id (accepted=false).
	// This is success, not an error.
	Duplicate bool
}

// Ingest records usage and debits the balance. A duplicate request_id is not an
// error: Talos returns 200 with accepted=false, surfaced via IngestResult.Duplicate.
func (c *TalosIngestClient) Ingest(ctx context.Context, req ingestRequest) (IngestResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return IngestResult{}, fmt.Errorf("marshal ingest request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+talosIngestPath, bytes.NewReader(body))
	if err != nil {
		return IngestResult{}, fmt.Errorf("build ingest request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.AdminToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.AdminToken)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return IngestResult{}, fmt.Errorf("talos ingest: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return IngestResult{}, fmt.Errorf("talos %s HTTP %d: %s",
			talosIngestPath, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out ingestResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return IngestResult{}, fmt.Errorf("decode talos response: %w (body=%q)", err, string(respBody))
	}
	return IngestResult{
		Accepted:         out.Accepted,
		BalanceRemaining: out.BalanceRemaining,
		BalanceQuota:     out.BalanceQuota,
		Duplicate:        !out.Accepted,
	}, nil
}
