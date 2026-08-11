use crate::pricing::Usage;
use sha2::{Digest, Sha256};
use std::collections::HashMap;

// Attribute keys - must match brightstaff's tracing/constants.rs
pub const ATTR_ACTOR_ID: &str = "billing.actor_id";
pub const ATTR_MODEL_ALIAS: &str = "billing.model_alias";
pub const ATTR_SESSION_ID: &str = "plano.session_id";
pub const ATTR_MODEL: &str = "llm.model";
pub const ATTR_PROMPT_TOKENS: &str = "llm.usage.prompt_tokens";
pub const ATTR_COMPLETION_TOKENS: &str = "llm.usage.completion_tokens";
pub const ATTR_TOTAL_TOKENS: &str = "llm.usage.total_tokens";
pub const ATTR_CACHED_TOKENS: &str = "llm.usage.cached_input_tokens";

/// Minimal OTLP types for span processing

#[derive(Debug, Clone)]
pub struct AnyValue {
    pub value: Option<AnyValueInner>,
}

#[derive(Debug, Clone)]
pub enum AnyValueInner {
    StringValue(String),
    IntValue(i64),
}

impl AnyValue {
    pub fn string_value(s: impl Into<String>) -> Self {
        Self {
            value: Some(AnyValueInner::StringValue(s.into())),
        }
    }

    pub fn int_value(v: i64) -> Self {
        Self {
            value: Some(AnyValueInner::IntValue(v)),
        }
    }

    pub fn get_string_value(&self) -> &str {
        match &self.value {
            Some(AnyValueInner::StringValue(s)) => s,
            _ => "",
        }
    }

    pub fn get_int_value(&self) -> i64 {
        match &self.value {
            Some(AnyValueInner::IntValue(v)) => *v,
            _ => 0,
        }
    }
}

#[derive(Debug, Clone)]
pub struct KeyValue {
    pub key: String,
    pub value: Option<AnyValue>,
}

#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct Span {
    pub trace_id: Vec<u8>,
    pub span_id: Vec<u8>,
    pub name: String,
    pub attributes: Vec<KeyValue>,
}

/// UsageEvent is the billable record derived from one LLM-completion span.
#[derive(Debug, Clone)]
pub struct UsageEvent {
    pub trace_id: String,
    pub span_id: String,
    pub actor_id: String,
    pub model: String,
    pub model_alias: String,
    pub session_id: String,
    pub usage: Usage,
    pub request_id: String,
}

/// leakInfo describes a billable-looking span that could not be metered.
#[derive(Debug, Clone)]
pub struct LeakInfo {
    pub reason: String,
    pub actor_id: String,
    pub model: String,
}

struct AttrMap(HashMap<String, AnyValue>);

impl AttrMap {
    fn str(&self, key: &str) -> &str {
        match self.0.get(key) {
            Some(v) => v.get_string_value(),
            None => "",
        }
    }

    fn int(&self, key: &str) -> i64 {
        match self.0.get(key) {
            Some(v) => v.get_int_value(),
            None => 0,
        }
    }
}

fn index_attrs(kvs: &[KeyValue]) -> AttrMap {
    let mut m = HashMap::with_capacity(kvs.len());
    for kv in kvs {
        if let Some(ref v) = kv.value {
            m.insert(kv.key.clone(), v.clone());
        }
    }
    AttrMap(m)
}

/// Derives a stable, <=36-char idempotency key from trace + span ids.
fn request_id(trace_id: &[u8], span_id: &[u8]) -> String {
    let mut h = Sha256::new();
    h.update(trace_id);
    h.update(span_id);
    let sum = h.finalize();
    hex::encode(&sum[..16]) // 32 hex chars
}

/// Maps a single OTLP span to a UsageEvent.
pub fn extract_event(span: &Span) -> Option<UsageEvent> {
    let attrs = index_attrs(&span.attributes);

    let actor = attrs.str(ATTR_ACTOR_ID);
    if actor.is_empty() {
        return None;
    }

    let model = attrs.str(ATTR_MODEL);
    if model.is_empty() {
        return None;
    }

    let mut prompt = attrs.int(ATTR_PROMPT_TOKENS);
    let completion = attrs.int(ATTR_COMPLETION_TOKENS);
    let total = attrs.int(ATTR_TOTAL_TOKENS);
    let mut cached = attrs.int(ATTR_CACHED_TOKENS);

    // Require at least one token signal
    if prompt == 0 && completion == 0 && total == 0 {
        return None;
    }

    // Reconcile partial reporting: treat total as prompt
    if prompt == 0 && completion == 0 && total > 0 {
        prompt = total;
    }
    if cached > prompt {
        cached = prompt;
    }

    let trace_hex = hex::encode(&span.trace_id);
    let span_hex = hex::encode(&span.span_id);

    Some(UsageEvent {
        trace_id: trace_hex,
        span_id: span_hex,
        actor_id: actor.to_string(),
        model: model.to_string(),
        model_alias: attrs.str(ATTR_MODEL_ALIAS).to_string(),
        session_id: attrs.str(ATTR_SESSION_ID).to_string(),
        usage: Usage {
            prompt_tokens: prompt,
            completion_tokens: completion,
            cached_input_tokens: cached,
        },
        request_id: request_id(&span.trace_id, &span.span_id),
    })
}

/// Classifies a span that extractEvent rejected.
pub fn span_leak(span: &Span) -> Option<LeakInfo> {
    let attrs = index_attrs(&span.attributes);

    let actor = attrs.str(ATTR_ACTOR_ID);
    if actor.is_empty() {
        return None; // not billable, not a leak
    }

    let model = attrs.str(ATTR_MODEL);
    if model.is_empty() {
        return Some(LeakInfo {
            reason: "missing_model".to_string(),
            actor_id: actor.to_string(),
            model: String::new(),
        });
    }

    if attrs.int(ATTR_PROMPT_TOKENS) == 0
        && attrs.int(ATTR_COMPLETION_TOKENS) == 0
        && attrs.int(ATTR_TOTAL_TOKENS) == 0
    {
        return Some(LeakInfo {
            reason: "missing_tokens".to_string(),
            actor_id: actor.to_string(),
            model: model.to_string(),
        });
    }

    None
}

#[cfg(test)]
mod tests {
    use super::*;

    fn billable_span(
        actor: &str,
        model: &str,
        prompt: i64,
        completion: i64,
        total: i64,
        cached: i64,
    ) -> Span {
        let mut attrs = vec![
            KeyValue {
                key: ATTR_ACTOR_ID.to_string(),
                value: Some(AnyValue::string_value(actor)),
            },
            KeyValue {
                key: ATTR_MODEL.to_string(),
                value: Some(AnyValue::string_value(model)),
            },
        ];
        if prompt != 0 {
            attrs.push(KeyValue {
                key: ATTR_PROMPT_TOKENS.to_string(),
                value: Some(AnyValue::int_value(prompt)),
            });
        }
        if completion != 0 {
            attrs.push(KeyValue {
                key: ATTR_COMPLETION_TOKENS.to_string(),
                value: Some(AnyValue::int_value(completion)),
            });
        }
        if total != 0 {
            attrs.push(KeyValue {
                key: ATTR_TOTAL_TOKENS.to_string(),
                value: Some(AnyValue::int_value(total)),
            });
        }
        if cached != 0 {
            attrs.push(KeyValue {
                key: ATTR_CACHED_TOKENS.to_string(),
                value: Some(AnyValue::int_value(cached)),
            });
        }
        Span {
            trace_id: vec![1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16],
            span_id: vec![17, 18, 19, 20, 21, 22, 23, 24],
            name: "POST /v1/chat/completions gpt-4".to_string(),
            attributes: attrs,
        }
    }

    #[test]
    fn test_extract_event_happy_path() {
        let span = billable_span("actor-001", "gpt-4", 120, 80, 200, 20);
        let ev = extract_event(&span).expect("expected extraction");
        assert_eq!(ev.actor_id, "actor-001");
        assert_eq!(ev.model, "gpt-4");
        assert_eq!(ev.usage.prompt_tokens, 120);
        assert_eq!(ev.usage.completion_tokens, 80);
        assert_eq!(ev.usage.cached_input_tokens, 20);
        assert!(!ev.trace_id.is_empty());
        assert!(!ev.span_id.is_empty());
        assert!(!ev.request_id.is_empty());
        assert!(ev.request_id.len() <= 36);
    }

    #[test]
    fn test_extract_event_request_id_stable() {
        let a = billable_span("a", "m", 1, 1, 2, 0);
        let b = billable_span("a", "m", 1, 1, 2, 0);
        let ea = extract_event(&a).unwrap();
        let eb = extract_event(&b).unwrap();
        assert_eq!(ea.request_id, eb.request_id);

        // Different span id -> different request_id
        let mut other = billable_span("a", "m", 1, 1, 2, 0);
        other.span_id = vec![99, 99, 99, 99, 99, 99, 99, 99];
        let eo = extract_event(&other).unwrap();
        assert_ne!(eo.request_id, ea.request_id);
    }

    #[test]
    fn test_extract_event_skips_missing_actor() {
        let span = billable_span("", "gpt-4", 1, 1, 2, 0);
        assert!(extract_event(&span).is_none());
    }

    #[test]
    fn test_extract_event_skips_missing_model() {
        let span = billable_span("actor", "", 1, 1, 2, 0);
        assert!(extract_event(&span).is_none());
    }

    #[test]
    fn test_extract_event_skips_no_tokens() {
        let span = Span {
            trace_id: vec![1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16],
            span_id: vec![17, 18, 19, 20, 21, 22, 23, 24],
            name: String::new(),
            attributes: vec![
                KeyValue {
                    key: ATTR_ACTOR_ID.to_string(),
                    value: Some(AnyValue::string_value("actor")),
                },
                KeyValue {
                    key: ATTR_MODEL.to_string(),
                    value: Some(AnyValue::string_value("gpt-4")),
                },
            ],
        };
        assert!(extract_event(&span).is_none());
    }

    #[test]
    fn test_extract_event_total_only_fallback() {
        let mut span = billable_span("actor", "gpt-4", 0, 0, 0, 0);
        span.attributes.push(KeyValue {
            key: ATTR_TOTAL_TOKENS.to_string(),
            value: Some(AnyValue::int_value(333)),
        });
        let ev = extract_event(&span).expect("expected total-only span to be billable");
        assert_eq!(ev.usage.prompt_tokens, 333);
    }

    #[test]
    fn test_span_leak() {
        // No actor_id => not a leak
        assert!(span_leak(&billable_span("", "gpt-4", 0, 0, 0, 0)).is_none());

        // Fully billable => not a leak
        assert!(span_leak(&billable_span("a", "gpt-4", 10, 5, 15, 0)).is_none());

        // actor_id but no model => leak
        let info = span_leak(&billable_span("a", "", 10, 5, 15, 0)).unwrap();
        assert_eq!(info.reason, "missing_model");
        assert_eq!(info.actor_id, "a");

        // actor_id + model but no tokens => leak
        let no_tokens = Span {
            trace_id: vec![],
            span_id: vec![],
            name: String::new(),
            attributes: vec![
                KeyValue {
                    key: ATTR_ACTOR_ID.to_string(),
                    value: Some(AnyValue::string_value("a")),
                },
                KeyValue {
                    key: ATTR_MODEL.to_string(),
                    value: Some(AnyValue::string_value("gpt-4")),
                },
            ],
        };
        let info = span_leak(&no_tokens).unwrap();
        assert_eq!(info.reason, "missing_tokens");
        assert_eq!(info.model, "gpt-4");
    }

    #[test]
    fn test_extract_event_clamps_cached_to_prompt() {
        let span = billable_span("actor", "gpt-4", 50, 0, 50, 999);
        let ev = extract_event(&span).unwrap();
        assert_eq!(ev.usage.cached_input_tokens, 50);
    }
}
