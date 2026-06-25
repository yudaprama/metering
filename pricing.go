package main

import (
	"math"
	"os"

	"gopkg.in/yaml.v3"
)

// ModelPricing is the per-model cost configuration. Prices are credits per
// 1_000_000 tokens. CacheDiscount is in [0,1]: 0.0 = cached tokens free,
// 1.0 = cached tokens charged at the full input rate. This mirrors the legacy
// brightstaff ModelPricing (Phase 3 removed pricing from brightstaff; it now
// lives exclusively here — see PLANO_AUTH_TO_ORY_PLAN.md Phase 6 / M3).
type ModelPricing struct {
	InputPerMillion  float64
	OutputPerMillion float64
	CacheDiscount    float64
}

// PricingConfig holds the default pricing plus per-model overrides.
type PricingConfig struct {
	Default ModelPricing
	Models  map[string]ModelPricing
}

// defaultPricingConfig returns the built-in fallback (input 5 / output 15 /
// cache_discount 0.5), matching brightstaff's ModelPricing::default().
func defaultPricingConfig() PricingConfig {
	return PricingConfig{
		Default: ModelPricing{InputPerMillion: 5.0, OutputPerMillion: 15.0, CacheDiscount: 0.5},
		Models:  map[string]ModelPricing{},
	}
}

// PricingFor returns the pricing for a model, falling back to Default when the
// model has no explicit override.
func (c PricingConfig) PricingFor(model string) ModelPricing {
	if p, ok := c.Models[model]; ok {
		return p
	}
	return c.Default
}

// Usage is the token usage extracted from an LLM-completion span.
type Usage struct {
	PromptTokens      int64
	CompletionTokens  int64
	CachedInputTokens int64
}

// CostMicros computes the cost in integer micros (cost x 1_000_000).
//
// Formula (PLANO_AUTH_TO_ORY_PLAN.md Phase 6 / M3):
//
//	non_cached_input = max(prompt - cached, 0)   charged at the full input rate
//	cached_input     = cached                    charged at input rate * cache_discount
//	output           = completion                charged at the output rate
//	cost = non_cached_input/1e6*in + cached_input/1e6*in*discount
//	       + output/1e6*out
//
// Storing money as integer micros matches the Talos fork's ledger
// (api_key_usage.cost_micros) and avoids float money math.
func (p ModelPricing) CostMicros(u Usage) int64 {
	// cached is a subset of prompt; clamp invalid usage data (cached > prompt,
	// negatives) so it can never produce a rebate or double-count tokens.
	cached := u.CachedInputTokens
	if cached > u.PromptTokens {
		cached = u.PromptTokens
	}
	if cached < 0 {
		cached = 0
	}
	nonCached := u.PromptTokens - cached
	input := float64(nonCached)/1e6*p.InputPerMillion +
		float64(cached)/1e6*p.InputPerMillion*p.CacheDiscount
	output := float64(u.CompletionTokens) / 1e6 * p.OutputPerMillion
	return int64(math.Round((input + output) * 1e6))
}

// rawModelPricing uses pointers so an omitted field can inherit a default
// (mirrors serde's per-field #[serde(default)] in the legacy Rust config).
type rawModelPricing struct {
	InputPerMillion  *float64 `yaml:"input_per_million"`
	OutputPerMillion *float64 `yaml:"output_per_million"`
	CacheDiscount    *float64 `yaml:"cache_discount"`
}

// rawPricingFile is the shape of metering/pricing.yaml:
//
//	default:
//	  input_per_million: 5.0
//	  output_per_million: 15.0
//	  cache_discount: 0.5
//	models:
//	  gpt-4: { input_per_million: 30.0, output_per_million: 60.0 }
type rawPricingFile struct {
	Default *rawModelPricing             `yaml:"default"`
	Models  map[string]rawModelPricing   `yaml:"models"`
}

// rawPlanoConfig is the shape of plano_config.yaml's (now dead) billing block,
// still honored as a pricing source so existing per-model overrides keep
// working without a separate config file:
//
//	billing:
//	  default_pricing: { input_per_million, output_per_million, cache_discount }
//	  pricing:
//	    <model>: { input_per_million, output_per_million }
type rawPlanoConfig struct {
	Billing *struct {
		DefaultPricing *rawModelPricing            `yaml:"default_pricing"`
		Pricing        map[string]rawModelPricing   `yaml:"pricing"`
	} `yaml:"billing"`
}

// loadPricingConfig resolves pricing in priority order:
//  1. METERING_PRICING_CONFIG file (if set and readable)
//  2. plano_config.yaml billing block (METERING_PLANO_CONFIG, else ./plano_config.yaml)
//  3. built-in defaults
//
// It never returns an error: pricing is required to bill, so on any read/parse
// failure it falls back to the next source and ultimately to the defaults.
func loadPricingConfig() PricingConfig {
	if path := os.Getenv("METERING_PRICING_CONFIG"); path != "" {
		if c, ok := loadPricingFile(path); ok {
			return c
		}
	}

	planoPath := os.Getenv("METERING_PLANO_CONFIG")
	if planoPath == "" {
		planoPath = "plano_config.yaml"
	}
	if c, ok := loadPlanoBilling(planoPath); ok {
		return c
	}

	return defaultPricingConfig()
}

func loadPricingFile(path string) (PricingConfig, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PricingConfig{}, false
	}
	var raw rawPricingFile
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return PricingConfig{}, false
	}

	base := defaultPricingConfig()
	cfg := PricingConfig{Default: base.Default, Models: map[string]ModelPricing{}}
	if raw.Default != nil {
		cfg.Default = applyPricingDefaults(*raw.Default, base.Default)
	}
	for model, r := range raw.Models {
		cfg.Models[model] = applyPricingDefaults(r, cfg.Default)
	}
	return cfg, true
}

func loadPlanoBilling(path string) (PricingConfig, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PricingConfig{}, false
	}
	var raw rawPlanoConfig
	if err := yaml.Unmarshal(b, &raw); err != nil || raw.Billing == nil {
		return PricingConfig{}, false
	}

	base := defaultPricingConfig()
	cfg := PricingConfig{Default: base.Default, Models: map[string]ModelPricing{}}
	if raw.Billing.DefaultPricing != nil {
		cfg.Default = applyPricingDefaults(*raw.Billing.DefaultPricing, base.Default)
	}
	for model, r := range raw.Billing.Pricing {
		cfg.Models[model] = applyPricingDefaults(r, cfg.Default)
	}
	return cfg, true
}

// applyPricingDefaults fills omitted (nil) fields from fallback, so a per-model
// override that lists only input/output inherits the default cache_discount.
func applyPricingDefaults(r rawModelPricing, fallback ModelPricing) ModelPricing {
	out := ModelPricing{
		InputPerMillion:  fallback.InputPerMillion,
		OutputPerMillion: fallback.OutputPerMillion,
		CacheDiscount:    fallback.CacheDiscount,
	}
	if r.InputPerMillion != nil {
		out.InputPerMillion = *r.InputPerMillion
	}
	if r.OutputPerMillion != nil {
		out.OutputPerMillion = *r.OutputPerMillion
	}
	if r.CacheDiscount != nil {
		out.CacheDiscount = *r.CacheDiscount
	}
	return out
}
