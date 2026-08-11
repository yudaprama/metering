use reqwest::Client;
use serde::{Deserialize, Serialize};
use std::env;
use std::time::Duration;

pub const TALOS_INGEST_PATH: &str = "/v2alpha1/admin/usage:ingest";
pub const USAGE_TYPE_TOKENS: &str = "tokens";

/// TalosIngestClient calls the Talos fork's AdminIngestUsage HTTP endpoint.
#[derive(Clone)]
pub struct TalosIngestClient {
    pub base_url: String,
    pub admin_token: String,
    pub http: Client,
}

impl TalosIngestClient {
    pub fn new() -> Self {
        let base = env::var("TALOS_URL")
            .unwrap_or_else(|_| "http://localhost:4420".to_string())
            .trim_end_matches('/')
            .to_string();

        Self {
            base_url: base,
            admin_token: env::var("TALOS_ADMIN_TOKEN").unwrap_or_default(),
            http: Client::builder()
                .timeout(Duration::from_secs(10))
                .build()
                .unwrap(),
        }
    }
}

/// ingestRequest is the protojson (camelCase) body for AdminIngestUsage.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct IngestRequest {
    #[serde(rename = "actorId")]
    pub actor_id: String,
    #[serde(rename = "keyId", skip_serializing_if = "Option::is_none")]
    pub key_id: Option<String>,
    #[serde(rename = "usageType")]
    pub usage_type: String,
    #[serde(rename = "usageAmount")]
    pub usage_amount: i64,
    #[serde(rename = "costMicros")]
    pub cost_micros: i64,
    #[serde(rename = "model")]
    pub model: String,
    #[serde(rename = "requestId")]
    pub request_id: String,
    #[serde(rename = "sessionId", skip_serializing_if = "Option::is_none")]
    pub session_id: Option<String>,
}

/// ingestResponse is the protojson (camelCase) body for IngestUsageResponse.
#[derive(Debug, Deserialize)]
struct IngestResponse {
    #[serde(rename = "balanceRemaining")]
    balance_remaining: i64,
    #[serde(rename = "balanceQuota")]
    balance_quota: i64,
    accepted: bool,
}

/// IngestResult reports the outcome of a debit.
#[derive(Debug)]
#[allow(dead_code)]
pub struct IngestResult {
    pub accepted: bool,
    pub balance_remaining: i64,
    pub balance_quota: i64,
    /// Duplicate is true when Talos reports a replayed request_id (accepted=false).
    pub duplicate: bool,
}

impl TalosIngestClient {
    /// Ingest records usage and debits the balance.
    pub async fn ingest(&self, req: &IngestRequest) -> Result<IngestResult, String> {
        let body = serde_json::to_vec(req).map_err(|e| format!("marshal ingest request: {e}"))?;

        let url = format!("{}{}", self.base_url, TALOS_INGEST_PATH);
        let mut builder = self
            .http
            .post(&url)
            .header("Content-Type", "application/json");

        if !self.admin_token.is_empty() {
            builder = builder.header("Authorization", format!("Bearer {}", self.admin_token));
        }

        let resp = builder
            .body(body)
            .send()
            .await
            .map_err(|e| format!("talos ingest: {e}"))?;

        let status = resp.status();
        let resp_body = resp
            .text()
            .await
            .map_err(|e| format!("read talos response: {e}"))?;

        if status.is_success() {
            let out: IngestResponse = serde_json::from_str(&resp_body)
                .map_err(|e| format!("decode talos response: {e} (body={resp_body:?})"))?;

            Ok(IngestResult {
                accepted: out.accepted,
                balance_remaining: out.balance_remaining,
                balance_quota: out.balance_quota,
                duplicate: !out.accepted,
            })
        } else {
            Err(format!(
                "talos {} HTTP {}: {}",
                TALOS_INGEST_PATH,
                status.as_u16(),
                resp_body.trim()
            ))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_ingest_request_serialization() {
        let req = IngestRequest {
            actor_id: "actor-1".to_string(),
            key_id: None,
            usage_type: USAGE_TYPE_TOKENS.to_string(),
            usage_amount: 200,
            cost_micros: 500,
            model: "gpt-4".to_string(),
            request_id: "abc123".to_string(),
            session_id: None,
        };

        let json = serde_json::to_string(&req).unwrap();
        assert!(json.contains("\"actorId\""));
        assert!(json.contains("\"usageType\""));
        assert!(json.contains("\"usageAmount\""));
        assert!(json.contains("\"costMicros\""));
        assert!(json.contains("\"requestId\""));
    }
}
