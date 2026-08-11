use serde::Deserialize;
use std::collections::HashMap;
use std::env;
use std::fs;

/// ModelPricing is the per-model cost configuration.
/// Prices are credits per 1_000_000 tokens.
#[derive(Debug, Clone, Deserialize)]
pub struct ModelPricing {
    #[serde(default)]
    pub input_per_million: f64,
    #[serde(default)]
    pub output_per_million: f64,
    #[serde(default = "default_cache_discount")]
    pub cache_discount: f64,
}

fn default_cache_discount() -> f64 {
    0.5
}

impl Default for ModelPricing {
    fn default() -> Self {
        Self {
            input_per_million: 0.0,
            output_per_million: 0.0,
            cache_discount: 0.5,
        }
    }
}

/// PricingConfig holds the default pricing plus per-model overrides.
#[derive(Debug, Clone)]
pub struct PricingConfig {
    pub default: ModelPricing,
    pub models: HashMap<String, ModelPricing>,
}

impl Default for PricingConfig {
    fn default() -> Self {
        Self::default_pricing()
    }
}

impl PricingConfig {
    /// Returns the built-in fallback (input 5 / output 15 / cache_discount 0.5).
    pub fn default_pricing() -> Self {
        Self {
            default: ModelPricing {
                input_per_million: 5.0,
                output_per_million: 15.0,
                cache_discount: 0.5,
            },
            models: HashMap::new(),
        }
    }

    /// Returns the pricing for a model, falling back to Default when the
    /// model has no explicit override.
    pub fn pricing_for(&self, model: &str) -> &ModelPricing {
        if let Some(p) = self.models.get(model) {
            return p;
        }
        if model.ends_with(":free") {
            return &FREE_PRICING;
        }
        &self.default
    }
}

static FREE_PRICING: ModelPricing = ModelPricing {
    input_per_million: 0.0,
    output_per_million: 0.0,
    cache_discount: 0.0,
};

/// Usage is the token usage extracted from an LLM-completion span.
#[derive(Debug, Clone, Default)]
pub struct Usage {
    pub prompt_tokens: i64,
    pub completion_tokens: i64,
    pub cached_input_tokens: i64,
}

impl ModelPricing {
    /// CostMicros computes the cost in integer micros (cost x 1_000_000).
    pub fn cost_micros(&self, u: &Usage) -> i64 {
        let mut cached = u.cached_input_tokens;
        if cached > u.prompt_tokens {
            cached = u.prompt_tokens;
        }
        if cached < 0 {
            cached = 0;
        }
        let non_cached = u.prompt_tokens - cached;
        let input = (non_cached as f64) / 1e6 * self.input_per_million
            + (cached as f64) / 1e6 * self.input_per_million * self.cache_discount;
        let output = (u.completion_tokens as f64) / 1e6 * self.output_per_million;
        ((input + output) * 1e6).round() as i64
    }
}

// YAML deserialization types

#[derive(Debug, Deserialize)]
struct RawModelPricing {
    input_per_million: Option<f64>,
    output_per_million: Option<f64>,
    cache_discount: Option<f64>,
}

#[derive(Debug, Deserialize)]
struct RawPricingFile {
    default: Option<RawModelPricing>,
    models: Option<HashMap<String, RawModelPricing>>,
}

#[derive(Debug, Deserialize)]
struct RawPlanoConfig {
    billing: Option<RawPlanoBilling>,
}

#[derive(Debug, Deserialize)]
struct RawPlanoBilling {
    default_pricing: Option<RawModelPricing>,
    pricing: Option<HashMap<String, RawModelPricing>>,
}

fn apply_pricing_defaults(r: &RawModelPricing, fallback: &ModelPricing) -> ModelPricing {
    ModelPricing {
        input_per_million: r.input_per_million.unwrap_or(fallback.input_per_million),
        output_per_million: r.output_per_million.unwrap_or(fallback.output_per_million),
        cache_discount: r.cache_discount.unwrap_or(fallback.cache_discount),
    }
}

fn load_pricing_file(path: &str) -> Option<PricingConfig> {
    let b = fs::read(path).ok()?;
    let raw: RawPricingFile = serde_yaml::from_slice(&b).ok()?;

    let base = PricingConfig::default_pricing();
    let mut cfg = PricingConfig {
        default: base.default.clone(),
        models: HashMap::new(),
    };

    if let Some(ref d) = raw.default {
        cfg.default = apply_pricing_defaults(d, &base.default);
    }

    if let Some(ref models) = raw.models {
        for (model, r) in models {
            cfg.models
                .insert(model.clone(), apply_pricing_defaults(r, &cfg.default));
        }
    }

    Some(cfg)
}

fn load_plano_billing(path: &str) -> Option<PricingConfig> {
    let b = fs::read(path).ok()?;
    let raw: RawPlanoConfig = serde_yaml::from_slice(&b).ok()?;
    let billing = raw.billing?;

    let base = PricingConfig::default_pricing();
    let mut cfg = PricingConfig {
        default: base.default.clone(),
        models: HashMap::new(),
    };

    if let Some(ref d) = billing.default_pricing {
        cfg.default = apply_pricing_defaults(d, &base.default);
    }

    if let Some(ref pricing) = billing.pricing {
        for (model, r) in pricing {
            cfg.models
                .insert(model.clone(), apply_pricing_defaults(r, &cfg.default));
        }
    }

    Some(cfg)
}

/// Loads pricing config in priority order:
/// 1. METERING_PRICING_CONFIG file
/// 2. plano_config.yaml billing block
/// 3. built-in defaults
pub fn load_pricing_config() -> PricingConfig {
    if let Ok(path) = env::var("METERING_PRICING_CONFIG") {
        if !path.is_empty() {
            if let Some(c) = load_pricing_file(&path) {
                return c;
            }
        }
    }

    let plano_path = env::var("METERING_PLANO_CONFIG")
        .unwrap_or_else(|_| "plano_config.yaml".to_string());

    if let Some(c) = load_plano_billing(&plano_path) {
        return c;
    }

    PricingConfig::default_pricing()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_default_pricing() {
        let cfg = PricingConfig::default_pricing();
        assert_eq!(cfg.default.input_per_million, 5.0);
        assert_eq!(cfg.default.output_per_million, 15.0);
        assert_eq!(cfg.default.cache_discount, 0.5);
    }

    #[test]
    fn test_pricing_for_override() {
        let mut models = HashMap::new();
        models.insert(
            "gpt-4".to_string(),
            ModelPricing {
                input_per_million: 30.0,
                output_per_million: 60.0,
                cache_discount: 0.5,
            },
        );
        let cfg = PricingConfig {
            default: ModelPricing {
                input_per_million: 5.0,
                output_per_million: 15.0,
                cache_discount: 0.5,
            },
            models,
        };

        let p = cfg.pricing_for("gpt-4");
        assert_eq!(p.input_per_million, 30.0);

        let p = cfg.pricing_for("unknown");
        assert_eq!(p.input_per_million, 5.0);
    }

    #[test]
    fn test_pricing_for_free_suffix() {
        let cfg = PricingConfig::default_pricing();
        let cases = vec![
            "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free",
            "openai/gpt-oss-120b:free",
            "meta-llama/llama-3.1-8b-instruct:free",
            ":free",
        ];
        for m in cases {
            let p = cfg.pricing_for(m);
            assert_eq!(p.input_per_million, 0.0);
            assert_eq!(p.output_per_million, 0.0);
            let cost = p.cost_micros(&Usage {
                prompt_tokens: 1_000_000,
                completion_tokens: 1_000_000,
                ..Default::default()
            });
            assert_eq!(cost, 0);
        }
        // Non-suffix model
        let p = cfg.pricing_for("some:freedom-7b");
        assert_eq!(p.input_per_million, 5.0);
    }

    #[test]
    fn test_cost_micros() {
        let p = ModelPricing {
            input_per_million: 5.0,
            output_per_million: 15.0,
            cache_discount: 0.5,
        };

        // Pure input 1M
        assert_eq!(
            p.cost_micros(&Usage {
                prompt_tokens: 1_000_000,
                ..Default::default()
            }),
            5_000_000
        );

        // Pure output 1M
        assert_eq!(
            p.cost_micros(&Usage {
                completion_tokens: 1_000_000,
                ..Default::default()
            }),
            15_000_000
        );

        // Input + output
        assert_eq!(
            p.cost_micros(&Usage {
                prompt_tokens: 1_000_000,
                completion_tokens: 1_000_000,
                ..Default::default()
            }),
            20_000_000
        );

        // Cached input discounted
        assert_eq!(
            p.cost_micros(&Usage {
                prompt_tokens: 1_000_000,
                cached_input_tokens: 1_000_000,
                ..Default::default()
            }),
            2_500_000
        );

        // Half cached
        assert_eq!(
            p.cost_micros(&Usage {
                prompt_tokens: 1_000_000,
                cached_input_tokens: 500_000,
                ..Default::default()
            }),
            3_750_000
        );

        // Zero usage
        assert_eq!(p.cost_micros(&Usage::default()), 0);
    }

    #[test]
    fn test_cost_micros_cached_clamps() {
        let p = ModelPricing {
            input_per_million: 5.0,
            output_per_million: 15.0,
            cache_discount: 0.5,
        };
        // cached > prompt is invalid; clamp cached to prompt (100)
        let cost = p.cost_micros(&Usage {
            prompt_tokens: 100,
            cached_input_tokens: 1_000,
            ..Default::default()
        });
        assert_eq!(cost, 250);
    }
}
