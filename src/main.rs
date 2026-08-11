mod enqueue;
mod extract;
mod pricing;
mod server;
mod talos;

use clap::Parser;
use pricing::load_pricing_config;
use server::MeteringServer;
use std::net::SocketAddr;
use std::sync::Arc;
use talos::TalosIngestClient;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio::signal;
use tracing::{error, info};
use tracing_subscriber::EnvFilter;

const VERSION: &str = env!("CARGO_PKG_VERSION");

#[derive(Parser)]
#[command(
    name = "metering",
    about = "OTLP/gRPC billing consumer for Plano metering"
)]
struct Args {
    /// Print version and exit
    #[arg(long)]
    version: bool,

    /// OTLP/gRPC trace receiver listen address
    #[arg(
        long,
        default_value = "127.0.0.1:4319",
        env = "METERING_OTLP_GRPC_ADDR"
    )]
    otlp_addr: SocketAddr,

    /// Healthz HTTP listen address
    #[arg(long, default_value = "127.0.0.1:4320", env = "METERING_HEALTH_ADDR")]
    health_addr: SocketAddr,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Initialize tracing
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::from_default_env().add_directive(tracing::Level::INFO.into()))
        .json()
        .init();

    let args = Args::parse();

    if args.version {
        println!("metering {VERSION}");
        return Ok(());
    }

    // Load pricing config
    let pricing = load_pricing_config();
    info!(
        default_input_per_million = pricing.default.input_per_million,
        default_output_per_million = pricing.default.output_per_million,
        default_cache_discount = pricing.default.cache_discount,
        model_overrides = pricing.models.len(),
        "metering pricing loaded"
    );

    // Create Talos client
    let talos = TalosIngestClient::new();
    info!(
        url = %format!("{}{}", talos.base_url, talos::TALOS_INGEST_PATH),
        auth = !talos.admin_token.is_empty(),
        "talos ingest target"
    );

    // Create debit enqueuer
    let enqueuer = enqueue::DebitEnqueuer::new();

    // Create metering server
    let srv = Arc::new(MeteringServer::new(pricing, talos, enqueuer));

    // Healthz HTTP server
    let health_srv = srv.clone();
    let health_addr = args.health_addr;
    tokio::spawn(async move {
        let listener = match TcpListener::bind(health_addr).await {
            Ok(l) => l,
            Err(e) => {
                error!(addr = %health_addr, error = %e, "health listen failed");
                return;
            }
        };
        info!(addr = %health_addr, "healthz HTTP listening");

        loop {
            let (mut stream, _) = match listener.accept().await {
                Ok(v) => v,
                Err(e) => {
                    error!(error = %e, "health accept failed");
                    continue;
                }
            };

            let snapshot = health_srv.snapshot();
            let body = serde_json::json!({
                "status": "ok",
                "version": VERSION,
                "metrics": snapshot,
            });

            let response = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{}",
                body.to_string().len(),
                body
            );

            let _ = stream.write_all(response.as_bytes()).await;
        }
    });

    // OTLP/gRPC server
    // NOTE: For full OTLP/gRPC support, integrate with tonic using the
    // opentelemetry-proto crate. This is a simplified HTTP-based receiver
    // for development/testing purposes.
    let otlp_srv = srv.clone();
    let otlp_addr = args.otlp_addr;
    tokio::spawn(async move {
        let listener = match TcpListener::bind(otlp_addr).await {
            Ok(l) => l,
            Err(e) => {
                error!(addr = %otlp_addr, error = %e, "otlp listen failed");
                return;
            }
        };
        info!(addr = %otlp_addr, "OTLP receiver listening");

        loop {
            let (mut stream, _) = match listener.accept().await {
                Ok(v) => v,
                Err(e) => {
                    error!(error = %e, "otlp accept failed");
                    continue;
                }
            };

            let srv = otlp_srv.clone();
            tokio::spawn(async move {
                let mut buf = vec![0u8; 65536];
                let n = match stream.read(&mut buf).await {
                    Ok(n) if n > 0 => n,
                    _ => return,
                };

                // Try to parse as JSON (simplified OTLP-like format)
                if let Ok(body) = std::str::from_utf8(&buf[..n]) {
                    if let Ok(json) = serde_json::from_str::<serde_json::Value>(body) {
                        // Extract spans from JSON (simplified parsing)
                        if let Some(spans) = json.get("spans").and_then(|s| s.as_array()) {
                            let otel_spans: Vec<extract::Span> =
                                spans.iter().filter_map(parse_otlp_span).collect();
                            srv.export(otel_spans).await;
                        }
                    }
                }

                let response = "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n";
                let _ = stream.write_all(response.as_bytes()).await;
            });
        }
    });

    info!(version = VERSION, "metering running");

    // Wait for shutdown signal
    signal::ctrl_c().await?;
    info!("shutting down metering...");
    tokio::time::sleep(std::time::Duration::from_secs(2)).await;
    info!("metering stopped");

    Ok(())
}

/// Parse a span from JSON (simplified OTLP format)
fn parse_otlp_span(json: &serde_json::Value) -> Option<extract::Span> {
    let trace_id = json.get("traceId")?.as_str()?;
    let span_id = json.get("spanId")?.as_str()?;
    let name = json.get("name").and_then(|n| n.as_str()).unwrap_or("");

    let mut attributes = Vec::new();
    if let Some(attrs) = json.get("attributes").and_then(|a| a.as_array()) {
        for attr in attrs {
            let key = attr.get("key")?.as_str()?;
            let value = attr.get("value")?;
            let any_value = if let Some(s) = value.get("stringValue") {
                extract::AnyValue::string_value(s.as_str().unwrap_or(""))
            } else if let Some(i) = value.get("intValue") {
                extract::AnyValue::int_value(i.as_i64().unwrap_or(0))
            } else {
                continue;
            };
            attributes.push(extract::KeyValue {
                key: key.to_string(),
                value: Some(any_value),
            });
        }
    }

    Some(extract::Span {
        trace_id: hex::decode(trace_id).unwrap_or_default(),
        span_id: hex::decode(span_id).unwrap_or_default(),
        name: name.to_string(),
        attributes,
    })
}
