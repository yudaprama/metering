use crate::enqueue::DebitEnqueuer;
use crate::extract::{extract_event, span_leak, Span};
use crate::pricing::PricingConfig;
use crate::talos::{IngestRequest, TalosIngestClient, USAGE_TYPE_TOKENS};
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::Arc;
use tracing::{error, info, warn};

/// MetricsSnapshot is a point-in-time copy of the counters.
#[derive(Debug, Clone, Default, serde::Serialize)]
pub struct MetricsSnapshot {
    pub spans_seen: i64,
    pub spans_billed: i64,
    pub spans_skipped: i64,
    pub debit_errors: i64,
    pub dedups: i64,
    pub retries_enqueued: i64,
    pub revenue_leaks: i64,
}

/// MeteringServer receives OTLP trace exports, extracts billable LLM-completion
/// spans, applies pricing, and debits the actor's balance via Talos.
#[derive(Clone)]
pub struct MeteringServer {
    pricing: PricingConfig,
    talos: TalosIngestClient,
    enqueuer: DebitEnqueuer,

    spans_seen: Arc<AtomicI64>,
    spans_billed: Arc<AtomicI64>,
    spans_skipped: Arc<AtomicI64>,
    debit_errors: Arc<AtomicI64>,
    dedups: Arc<AtomicI64>,
    retries_enqueued: Arc<AtomicI64>,
    revenue_leaks: Arc<AtomicI64>,
}

impl MeteringServer {
    pub fn new(pricing: PricingConfig, talos: TalosIngestClient, enqueuer: DebitEnqueuer) -> Self {
        Self {
            pricing,
            talos,
            enqueuer,
            spans_seen: Arc::new(AtomicI64::new(0)),
            spans_billed: Arc::new(AtomicI64::new(0)),
            spans_skipped: Arc::new(AtomicI64::new(0)),
            debit_errors: Arc::new(AtomicI64::new(0)),
            dedups: Arc::new(AtomicI64::new(0)),
            retries_enqueued: Arc::new(AtomicI64::new(0)),
            revenue_leaks: Arc::new(AtomicI64::new(0)),
        }
    }

    /// Process a batch of spans (called from gRPC Export handler).
    pub async fn export(&self, spans: Vec<Span>) {
        for span in spans {
            self.handle_span(span).await;
        }
    }

    async fn handle_span(&self, span: Span) {
        self.spans_seen.fetch_add(1, Ordering::SeqCst);

        let ev = match extract_event(&span) {
            Some(ev) => ev,
            None => {
                self.spans_skipped.fetch_add(1, Ordering::SeqCst);

                // Check for revenue leak
                if let Some(leak) = span_leak(&span) {
                    self.revenue_leaks.fetch_add(1, Ordering::SeqCst);
                    warn!(
                        reason = %leak.reason,
                        actor_id = %leak.actor_id,
                        model = %leak.model,
                        trace_id = %hex::encode(&span.trace_id),
                        span_id = %hex::encode(&span.span_id),
                        "billable span could not be metered (revenue leak)"
                    );
                }
                return;
            }
        };

        let pricing = self.pricing.pricing_for(&ev.model);
        let cost_micros = pricing.cost_micros(&ev.usage);
        let usage_amount = ev.usage.prompt_tokens + ev.usage.completion_tokens;

        // Use model alias for ledger if present
        let ledger_model = if ev.model_alias.is_empty() {
            ev.model.clone()
        } else {
            ev.model_alias.clone()
        };

        let req = IngestRequest {
            actor_id: ev.actor_id.clone(),
            key_id: None,
            usage_type: USAGE_TYPE_TOKENS.to_string(),
            usage_amount,
            cost_micros,
            model: ledger_model.clone(),
            request_id: ev.request_id.clone(),
            session_id: if ev.session_id.is_empty() {
                None
            } else {
                Some(ev.session_id.clone())
            },
        };

        match self.talos.ingest(&req).await {
            Ok(res) => {
                self.spans_billed.fetch_add(1, Ordering::SeqCst);
                if res.duplicate {
                    self.dedups.fetch_add(1, Ordering::SeqCst);
                }
                info!(
                    actor_id = %ev.actor_id,
                    model = %ledger_model,
                    resolved_model = %ev.model,
                    prompt = ev.usage.prompt_tokens,
                    completion = ev.usage.completion_tokens,
                    cached = ev.usage.cached_input_tokens,
                    usage_amount = usage_amount,
                    cost_micros = cost_micros,
                    balance_remaining = res.balance_remaining,
                    balance_quota = res.balance_quota,
                    duplicate = res.duplicate,
                    trace_id = %ev.trace_id,
                    span_id = %ev.span_id,
                    "billed usage"
                );
            }
            Err(err) => {
                self.debit_errors.fetch_add(1, Ordering::SeqCst);

                // Hand to enqueuer for durable retry
                self.enqueuer.enqueue_debit(req);
                self.retries_enqueued.fetch_add(1, Ordering::SeqCst);

                error!(
                    actor_id = %ev.actor_id,
                    model = %ev.model,
                    trace_id = %ev.trace_id,
                    span_id = %ev.span_id,
                    cost_micros = cost_micros,
                    error = %err,
                    "debit failed"
                );
            }
        }
    }

    pub fn snapshot(&self) -> MetricsSnapshot {
        MetricsSnapshot {
            spans_seen: self.spans_seen.load(Ordering::SeqCst),
            spans_billed: self.spans_billed.load(Ordering::SeqCst),
            spans_skipped: self.spans_skipped.load(Ordering::SeqCst),
            debit_errors: self.debit_errors.load(Ordering::SeqCst),
            dedups: self.dedups.load(Ordering::SeqCst),
            retries_enqueued: self.retries_enqueued.load(Ordering::SeqCst),
            revenue_leaks: self.revenue_leaks.load(Ordering::SeqCst),
        }
    }
}
