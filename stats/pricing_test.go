package stats

import (
	"encoding/json"
	"testing"
)

func TestNewPricingCacheStartsWithBuiltinGPT56Pricing(t *testing.T) {
	cache := newPricingCache(t.TempDir())

	got := cache.LookupPrice("gpt-5.6-luna")
	want := builtinPricing["gpt-5.6-luna"]
	if got == nil {
		t.Fatal("missing built-in pricing before remote fetch")
	}
	if *got != want {
		t.Fatalf("pricing = %+v, want %+v", *got, want)
	}
}

func TestApplyBuiltinPricingAddsGPT56Series(t *testing.T) {
	data := map[string]json.RawMessage{}
	applyBuiltinPricing(data)

	tests := map[string]ModelPricing{
		"gpt-5.6":       {InputCostPerToken: 0.000005, CacheReadCostPerToken: 0.0000005, OutputCostPerToken: 0.00003},
		"gpt-5.6-sol":   {InputCostPerToken: 0.000005, CacheReadCostPerToken: 0.0000005, OutputCostPerToken: 0.00003},
		"gpt-5.6-terra": {InputCostPerToken: 0.0000025, CacheReadCostPerToken: 0.00000025, OutputCostPerToken: 0.000015},
		"gpt-5.6-luna":  {InputCostPerToken: 0.000001, CacheReadCostPerToken: 0.0000001, OutputCostPerToken: 0.000006},
	}

	for model, want := range tests {
		t.Run(model, func(t *testing.T) {
			var got ModelPricing
			if err := json.Unmarshal(data[model], &got); err != nil {
				t.Fatalf("unmarshal pricing: %v", err)
			}
			if got != want {
				t.Fatalf("pricing = %+v, want %+v", got, want)
			}
		})
	}
}

func TestApplyBuiltinPricingOverridesCostsAndPreservesMetadata(t *testing.T) {
	data := map[string]json.RawMessage{
		"gpt-5.6-terra": json.RawMessage(`{
			"input_cost_per_token": 1,
			"cache_read_input_token_cost": 2,
			"output_cost_per_token": 3,
			"max_tokens": 128000
		}`),
	}

	applyBuiltinPricing(data)

	var entry map[string]any
	if err := json.Unmarshal(data["gpt-5.6-terra"], &entry); err != nil {
		t.Fatalf("unmarshal pricing entry: %v", err)
	}
	if entry["input_cost_per_token"] != 0.0000025 {
		t.Fatalf("input cost = %v, want %v", entry["input_cost_per_token"], 0.0000025)
	}
	if entry["cache_read_input_token_cost"] != 0.00000025 {
		t.Fatalf("cached input cost = %v, want %v", entry["cache_read_input_token_cost"], 0.00000025)
	}
	if entry["output_cost_per_token"] != 0.000015 {
		t.Fatalf("output cost = %v, want %v", entry["output_cost_per_token"], 0.000015)
	}
	if entry["max_tokens"] != float64(128000) {
		t.Fatalf("max_tokens = %v, want 128000", entry["max_tokens"])
	}
}

func TestLookupPricesIncludesBuiltinGPT56Pricing(t *testing.T) {
	data := map[string]json.RawMessage{}
	applyBuiltinPricing(data)
	cache := &PricingCache{data: data}

	result := cache.LookupPrices([]string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"})
	if len(result) != 3 {
		t.Fatalf("pricing entries = %d, want 3", len(result))
	}
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if _, ok := result[model]; !ok {
			t.Errorf("missing pricing for %s", model)
		}
	}
}
