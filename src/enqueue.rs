use crate::talos::IngestRequest;
use reqwest::Client;
use std::env;
use std::time::Duration;
use tokio::sync::mpsc;
use tracing::{info, warn};

/// DebitEnqueuer forwards failed Talos debits to the egent-jobs trigger
/// endpoint for durable retry. It is fire-and-forget.
#[derive(Clone)]
#[allow(dead_code)]
pub struct DebitEnqueuer {
    url: String,
    http: Client,
    enabled: bool,
    tx: mpsc::UnboundedSender<IngestRequest>,
}

impl DebitEnqueuer {
    pub fn new() -> Self {
        let base = env::var("EGENT_JOBS_URL").unwrap_or_default();
        let base = base.trim_end_matches('/').to_string();

        let (tx, rx) = mpsc::unbounded_channel::<IngestRequest>();

        if base.is_empty() {
            info!("debit retry enqueuer disabled (set EGENT_JOBS_URL to enable)");
            return Self {
                url: String::new(),
                http: Client::new(),
                enabled: false,
                tx,
            };
        }

        let url = format!("{base}/trigger/debit-retry");
        let http = Client::builder()
            .timeout(Duration::from_secs(3))
            .build()
            .unwrap();

        let enqueuer = Self {
            url: url.clone(),
            http: http.clone(),
            enabled: true,
            tx,
        };

        // Spawn background worker
        tokio::spawn(async move {
            Self::run_worker(url, http, rx).await;
        });

        enqueuer
    }

    async fn run_worker(url: String, http: Client, mut rx: mpsc::UnboundedReceiver<IngestRequest>) {
        while let Some(req) = rx.recv().await {
            if let Err(e) = Self::send_debit(&url, &http, &req).await {
                warn!(
                    requestId = %req.request_id,
                    error = %e,
                    "debit enqueue failed"
                );
            }
        }
    }

    async fn send_debit(url: &str, http: &Client, req: &IngestRequest) -> Result<(), String> {
        let resp = http
            .post(url)
            .header("Content-Type", "application/json")
            .json(req)
            .send()
            .await
            .map_err(|e| format!("post failed: {e}"))?;

        if resp.status().as_u16() != 202 {
            return Err(format!("non-202 from egent-jobs: {}", resp.status()));
        }

        info!(
            requestId = %req.request_id,
            actorId = %req.actor_id,
            "debit enqueued for durable retry"
        );

        Ok(())
    }

    /// Enqueue a failed debit for retry. Fire-and-forget.
    pub fn enqueue_debit(&self, req: IngestRequest) {
        if !self.enabled {
            return;
        }
        let _ = self.tx.send(req);
    }
}
