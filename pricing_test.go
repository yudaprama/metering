package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPricingConfig(t *testing.T) {
	cfg := defaultPricingConfig()
	if cfg.Default.InputPerMillion != 5.0 || cfg.Default.OutputPerMillion != 15.0 || cfg.Default.CacheDiscount != 0.5 {
		t.Fatalf("unexpected default: %+v", cfg.Default)
	}
	if cfg.Models == nil {
		t.Fatal("Models map should be initialized")
	}
}

func TestPricingFor(t *testing.T) {
	cfg := PricingConfig{
		Default: ModelPricing{InputPerMillion: 5.0, OutputPerMillion: 15.0, CacheDiscount: 0.5},
		Models: map[string]ModelPricing{
			"gpt-4": {InputPerMillion: 30.0, OutputPerMillion: 60.0, CacheDiscount: 0.5},
		},
	}
	if got := cfg.PricingFor("gpt-4"); got.InputPerMillion != 30.0 {
		t.Errorf("override not applied: %+v", got)
	}
	if got := cfg.PricingFor("unknown"); got.InputPerMillion != 5.0 {
		t.Errorf("default fallback not applied: %+v", got)
	}
}

func TestCostMicros(t *testing.T) {
	in := ModelPricing{InputPerMillion: 5.0, OutputPerMillion: 15.0, CacheDiscount: 0.5}

	cases := []struct {
		name string
		u    Usage
		want int64 // expected cost_micros
	}{
		{
			name: "pure input 1M",
			u:    Usage{PromptTokens: 1_000_000},
			// 1e6/1e6*5 = 5.0 -> 5_000_000 micros
			want: 5_000_000,
		},
		{
			name: "pure output 1M",
			u:    Usage{CompletionTokens: 1_000_000},
			// 1e6/1e6*15 = 15.0 -> 15_000_000 micros
			want: 15_000_000,
		},
		{
			name: "input+output",
			u:    Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000},
			want: 20_000_000,
		},
		{
			name: "cached input discounted",
			// 1M prompt all cached: 1e6/1e6*5*0.5 = 2.5 -> 2_500_000
			u:    Usage{PromptTokens: 1_000_000, CachedInputTokens: 1_000_000},
			want: 2_500_000,
		},
		{
			name: "half cached",
			// 500k non-cached @5 = 2.5 ; 500k cached @5*0.5 = 1.25 ; total 3.75 -> 3_750_000
			u:    Usage{PromptTokens: 1_000_000, CachedInputTokens: 500_000},
			want: 3_750_000,
		},
		{
			name: "zero usage",
			u:    Usage{},
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := in.CostMicros(tc.u)
			if got != tc.want {
				t.Errorf("CostMicros(%+v) = %d, want %d", tc.u, got, tc.want)
			}
		})
	}
}

func TestCostMicrosCachedClampsNegative(t *testing.T) {
	in := ModelPricing{InputPerMillion: 5.0, OutputPerMillion: 15.0, CacheDiscount: 0.5}
	// cached > prompt is invalid usage data; clamp cached to prompt (100), so all
	// 100 prompt tokens are charged at the cached (discount) rate: 100/1e6*5*0.5.
	got := in.CostMicros(Usage{PromptTokens: 100, CachedInputTokens: 1_000})
	if got < 0 {
		t.Errorf("expected non-negative cost, got %d", got)
	}
	if got != 250 {
		t.Errorf("expected 250 micros (100 cached tokens @5/M x0.5), got %d", got)
	}
}

func TestLoadPricingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	content := `
default:
  input_per_million: 3.0
  output_per_million: 9.0
  cache_discount: 0.25
models:
  gpt-4:
    input_per_million: 30.0
    output_per_million: 60.0
  cheap:
    input_per_million: 1.0
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, ok := loadPricingFile(path)
	if !ok {
		t.Fatal("expected load to succeed")
	}
	if cfg.Default.InputPerMillion != 3.0 || cfg.Default.OutputPerMillion != 9.0 || cfg.Default.CacheDiscount != 0.25 {
		t.Fatalf("default not parsed: %+v", cfg.Default)
	}
	// Full override.
	if got := cfg.PricingFor("gpt-4"); got.InputPerMillion != 30.0 || got.OutputPerMillion != 60.0 {
		t.Errorf("gpt-4 override wrong: %+v", got)
	}
	// Partial override: cheap sets only input -> output + cache_discount inherit default.
	got := cfg.PricingFor("cheap")
	if got.InputPerMillion != 1.0 {
		t.Errorf("cheap input wrong: %v", got.InputPerMillion)
	}
	if got.OutputPerMillion != 9.0 {
		t.Errorf("cheap output should inherit default 9.0, got %v", got.OutputPerMillion)
	}
	if got.CacheDiscount != 0.25 {
		t.Errorf("cheap cache_discount should inherit default 0.25, got %v", got.CacheDiscount)
	}
}

func TestLoadPlanoBilling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plano_config.yaml")
	content := `
billing:
  default_pricing:
    input_per_million: 5.0
    output_per_million: 15.0
    cache_discount: 0.5
  pricing:
    gpt-4:
      input_per_million: 30.0
      output_per_million: 60.0
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, ok := loadPlanoBilling(path)
	if !ok {
		t.Fatal("expected load to succeed")
	}
	if cfg.Default.InputPerMillion != 5.0 || cfg.Default.CacheDiscount != 0.5 {
		t.Fatalf("default not parsed: %+v", cfg.Default)
	}
	if got := cfg.PricingFor("gpt-4"); got.InputPerMillion != 30.0 || got.OutputPerMillion != 60.0 {
		t.Errorf("gpt-4 override wrong: %+v", got)
	}
	// Per-model override omits cache_discount -> inherits default 0.5.
	if got := cfg.PricingFor("gpt-4"); got.CacheDiscount != 0.5 {
		t.Errorf("gpt-4 cache_discount should inherit default 0.5, got %v", got.CacheDiscount)
	}
}

func TestLoadPlanoBillingNoBillingBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plano_config.yaml")
	content := `
listeners:
  - port: 12000
    type: model
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadPlanoBilling(path); ok {
		t.Fatal("expected load to report no billing block")
	}
}

func TestLoadPricingConfigFallbackToDefault(t *testing.T) {
	t.Setenv("METERING_PRICING_CONFIG", "/nonexistent/pricing.yaml")
	t.Setenv("METERING_PLANO_CONFIG", "/nonexistent/plano_config.yaml")
	cfg := loadPricingConfig()
	if cfg.Default.InputPerMillion != 5.0 || cfg.Default.OutputPerMillion != 15.0 || cfg.Default.CacheDiscount != 0.5 {
		t.Fatalf("did not fall back to built-in defaults: %+v", cfg.Default)
	}
}
