package main

import (
	"crypto/sha256"
	"encoding/hex"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Attribute keys. These MUST match brightstaff's tracing/constants.rs
// (tracing::llm::* and tracing::billing::ACTOR_ID).
const (
	attrActorID          = "billing.actor_id"
	attrModel            = "llm.model"
	attrPromptTokens     = "llm.usage.prompt_tokens"
	attrCompletionTokens = "llm.usage.completion_tokens"
	attrTotalTokens      = "llm.usage.total_tokens"
	attrCachedTokens     = "llm.usage.cached_input_tokens"
)

// UsageEvent is the billable record derived from one LLM-completion span.
type UsageEvent struct {
	TraceID string
	SpanID  string
	ActorID string
	Model   string
	Usage   Usage
	// RequestID is the stable idempotency key for Talos AdminIngestUsage.
	RequestID string
}

// extractEvent maps a single OTLP span to a UsageEvent. It returns ok=false for
// spans that are not billable LLM completions (missing actor_id, model, or any
// token count). This is the authoritative billability filter — the Alloy span
// filter is only a best-effort volume reducer; metering never trusts it alone.
func extractEvent(span *tracev1.Span) (UsageEvent, bool) {
	attrs := indexAttrs(span.GetAttributes())

	actor := attrs.str(attrActorID)
	if actor == "" {
		return UsageEvent{}, false
	}
	model := attrs.str(attrModel)
	if model == "" {
		return UsageEvent{}, false
	}

	prompt := attrs.int(attrPromptTokens)
	completion := attrs.int(attrCompletionTokens)
	total := attrs.int(attrTotalTokens)
	cached := attrs.int(attrCachedTokens)

	// Require at least one token signal so non-LLM spans that happen to carry
	// an actor_id (e.g. a routing span) are not billed.
	if prompt == 0 && completion == 0 && total == 0 {
		return UsageEvent{}, false
	}

	// Reconcile partial reporting: a span may carry only total_tokens
	// (some providers omit the prompt/completion split). Treat the total as
	// prompt so it is charged at the (usually cheaper) input rate rather than
	// guessing a split.
	if prompt == 0 && completion == 0 && total > 0 {
		prompt = total
	}
	if cached > prompt {
		cached = prompt
	}

	traceHex := hex.EncodeToString(span.GetTraceId())
	spanHex := hex.EncodeToString(span.GetSpanId())

	return UsageEvent{
		TraceID: traceHex,
		SpanID:  spanHex,
		ActorID: actor,
		Model:   model,
		Usage: Usage{
			PromptTokens:      prompt,
			CompletionTokens:  completion,
			CachedInputTokens: cached,
		},
		RequestID: requestID(span.GetTraceId(), span.GetSpanId()),
	}, true
}

// requestID derives a stable, <=36-char idempotency key from the trace + span
// ids. brightstaff's LLM span carries no native request_id, and OTLP spans are
// immutable per LLM call, so (trace_id, span_id) uniquely identifies the billed
// event. Alloy retries deliver the identical span, so this key makes
// AdminIngestUsage dedup correctly and prevents double-debit on replay.
// Talos validates request_id max_len = 36, so the key is truncated to 32 hex.
func requestID(traceID, spanID []byte) string {
	h := sha256.New()
	h.Write(traceID)
	h.Write(spanID)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16]) // 32 hex chars
}

type attrMap map[string]*commonv1.AnyValue

func indexAttrs(kvs []*commonv1.KeyValue) attrMap {
	m := make(attrMap, len(kvs))
	for _, kv := range kvs {
		if kv == nil {
			continue
		}
		m[kv.Key] = kv.Value
	}
	return m
}

func (m attrMap) str(key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	return v.GetStringValue()
}

func (m attrMap) int(key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	return v.GetIntValue()
}
