package main

import (
	"testing"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

func strVal(s string) *commonv1.AnyValue {
	return &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: s}}
}

func intVal(v int64) *commonv1.AnyValue {
	return &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: v}}
}

func kv(k string, v *commonv1.AnyValue) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: v}
}

func billableSpan(actor, model string, prompt, completion, total, cached int64) *tracev1.Span {
	attrs := []*commonv1.KeyValue{
		kv(attrActorID, strVal(actor)),
		kv(attrModel, strVal(model)),
	}
	if prompt != 0 {
		attrs = append(attrs, kv(attrPromptTokens, intVal(prompt)))
	}
	if completion != 0 {
		attrs = append(attrs, kv(attrCompletionTokens, intVal(completion)))
	}
	if total != 0 {
		attrs = append(attrs, kv(attrTotalTokens, intVal(total)))
	}
	if cached != 0 {
		attrs = append(attrs, kv(attrCachedTokens, intVal(cached)))
	}
	return &tracev1.Span{
		TraceId:    []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:     []byte{17, 18, 19, 20, 21, 22, 23, 24},
		Name:       "POST /v1/chat/completions gpt-4",
		Attributes: attrs,
	}
}

func TestExtractEventHappyPath(t *testing.T) {
	span := billableSpan("actor-001", "gpt-4", 120, 80, 200, 20)
	ev, ok := extractEvent(span)
	if !ok {
		t.Fatal("expected extraction to succeed")
	}
	if ev.ActorID != "actor-001" {
		t.Errorf("actor: got %q", ev.ActorID)
	}
	if ev.Model != "gpt-4" {
		t.Errorf("model: got %q", ev.Model)
	}
	if ev.Usage.PromptTokens != 120 || ev.Usage.CompletionTokens != 80 || ev.Usage.CachedInputTokens != 20 {
		t.Errorf("usage wrong: %+v", ev.Usage)
	}
	if ev.TraceID == "" || ev.SpanID == "" {
		t.Error("trace/span ids should be populated")
	}
	if len(ev.RequestID) > 36 || ev.RequestID == "" {
		t.Errorf("request_id must be 1..36 chars, got %q (len %d)", ev.RequestID, len(ev.RequestID))
	}
}

func TestExtractEventRequestIDStable(t *testing.T) {
	a := billableSpan("a", "m", 1, 1, 2, 0)
	b := billableSpan("a", "m", 1, 1, 2, 0)
	ea, _ := extractEvent(a)
	eb, _ := extractEvent(b)
	if ea.RequestID != eb.RequestID {
		t.Errorf("identical spans must yield identical request_id: %q vs %q", ea.RequestID, eb.RequestID)
	}
	// Different span id -> different request_id.
	other := billableSpan("a", "m", 1, 1, 2, 0)
	other.SpanId = []byte{99, 99, 99, 99, 99, 99, 99, 99}
	eo, _ := extractEvent(other)
	if eo.RequestID == ea.RequestID {
		t.Errorf("different spans must yield different request_id")
	}
}

func TestExtractEventSkipsMissingActor(t *testing.T) {
	span := billableSpan("", "gpt-4", 1, 1, 2, 0) // actor empty
	if _, ok := extractEvent(span); ok {
		t.Fatal("must skip when actor_id missing/empty")
	}
}

func TestExtractEventSkipsMissingModel(t *testing.T) {
	span := billableSpan("actor", "", 1, 1, 2, 0)
	if _, ok := extractEvent(span); ok {
		t.Fatal("must skip when model missing")
	}
}

func TestExtractEventSkipsNoTokens(t *testing.T) {
	// Span with actor+model but zero token attributes -> not billable.
	span := &tracev1.Span{
		TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:  []byte{17, 18, 19, 20, 21, 22, 23, 24},
		Attributes: []*commonv1.KeyValue{
			kv(attrActorID, strVal("actor")),
			kv(attrModel, strVal("gpt-4")),
		},
	}
	if _, ok := extractEvent(span); ok {
		t.Fatal("must skip spans with no token signal")
	}
}

func TestExtractEventTotalOnlyFallback(t *testing.T) {
	// Only total_tokens reported: treat as prompt (charged at input rate).
	span := &tracev1.Span{
		TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:  []byte{17, 18, 19, 20, 21, 22, 23, 24},
		Attributes: []*commonv1.KeyValue{
			kv(attrActorID, strVal("actor")),
			kv(attrModel, strVal("gpt-4")),
			kv(attrTotalTokens, intVal(333)),
		},
	}
	ev, ok := extractEvent(span)
	if !ok {
		t.Fatal("expected total-only span to be billable")
	}
	if ev.Usage.PromptTokens != 333 {
		t.Errorf("total-only should set prompt=total, got %d", ev.Usage.PromptTokens)
	}
}

func TestExtractEventClampsCachedToPrompt(t *testing.T) {
	span := billableSpan("actor", "gpt-4", 50, 0, 50, 999) // cached > prompt
	ev, ok := extractEvent(span)
	if !ok {
		t.Fatal("expected extraction")
	}
	if ev.Usage.CachedInputTokens != 50 {
		t.Errorf("cached should clamp to prompt, got %d", ev.Usage.CachedInputTokens)
	}
}
