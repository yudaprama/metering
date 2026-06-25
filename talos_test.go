package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, status int, resp string, check func(t *testing.T, r *http.Request)) *TalosIngestClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(t, r)
		}
		body, _ := io.ReadAll(r.Body)
		var req ingestRequest
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return &TalosIngestClient{
		BaseURL:    srv.URL,
		AdminToken: "test-admin-token",
		HTTP:       srv.Client(),
	}
}

func TestIngestSuccess(t *testing.T) {
	var got ingestRequest
	c := newTestServer(t, http.StatusOK,
		`{"balanceRemaining":9500000,"balanceQuota":10000000,"accepted":true}`,
		func(t *testing.T, r *http.Request) {
			if r.URL.Path != talosIngestPath {
				t.Errorf("path: got %q want %q", r.URL.Path, talosIngestPath)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-admin-token" {
				t.Errorf("admin token header not sent: got %q", got)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type: got %q", ct)
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &got)
		})

	res, err := c.Ingest(context.Background(), ingestRequest{
		ActorID:     "actor-1",
		UsageType:   usageTypeTokens,
		UsageAmount: 200,
		CostMicros:  500,
		Model:       "gpt-4",
		RequestID:   "abc123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Accepted || res.Duplicate {
		t.Errorf("expected accepted+non-duplicate, got %+v", res)
	}
	if res.BalanceRemaining != 9500000 || res.BalanceQuota != 10000000 {
		t.Errorf("balance fields wrong: %+v", res)
	}
	// Verify the wire body used camelCase field names.
	if got.ActorID != "actor-1" {
		t.Errorf("body actorId wrong: %+v", got)
	}
	if got.UsageAmount != 200 || got.CostMicros != 500 || got.Model != "gpt-4" || got.RequestID != "abc123" {
		t.Errorf("body fields wrong: %+v", got)
	}
}

func TestIngestDuplicate(t *testing.T) {
	c := newTestServer(t, http.StatusOK,
		`{"balanceRemaining":100,"balanceQuota":0,"accepted":false}`, nil)
	res, err := c.Ingest(context.Background(), ingestRequest{ActorID: "a", RequestID: "dup"})
	if err != nil {
		t.Fatalf("duplicate must not be an error: %v", err)
	}
	if !res.Duplicate {
		t.Errorf("expected Duplicate=true, got %+v", res)
	}
	if res.Accepted {
		t.Errorf("expected Accepted=false for duplicate, got %+v", res)
	}
}

func TestIngestHTTPError(t *testing.T) {
	c := newTestServer(t, http.StatusInternalServerError, `{"message":"boom"}`, nil)
	_, err := c.Ingest(context.Background(), ingestRequest{ActorID: "a"})
	if err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestIngestNoAdminToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("no Authorization header expected when token unset, got %q", auth)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	t.Cleanup(srv.Close)
	c := &TalosIngestClient{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.Ingest(context.Background(), ingestRequest{ActorID: "a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
