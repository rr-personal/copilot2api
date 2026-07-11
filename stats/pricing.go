package stats

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	litellmPricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	pricingCacheFile  = "pricing_cache.json"
)

var builtinPricing = map[string]ModelPricing{
	"gpt-5.6": {
		InputCostPerToken:     5.0 / 1_000_000,
		CacheReadCostPerToken: 0.5 / 1_000_000,
		OutputCostPerToken:    30.0 / 1_000_000,
	},
	"gpt-5.6-sol": {
		InputCostPerToken:     5.0 / 1_000_000,
		CacheReadCostPerToken: 0.5 / 1_000_000,
		OutputCostPerToken:    30.0 / 1_000_000,
	},
	"gpt-5.6-terra": {
		InputCostPerToken:     2.5 / 1_000_000,
		CacheReadCostPerToken: 0.25 / 1_000_000,
		OutputCostPerToken:    15.0 / 1_000_000,
	},
	"gpt-5.6-luna": {
		InputCostPerToken:     1.0 / 1_000_000,
		CacheReadCostPerToken: 0.1 / 1_000_000,
		OutputCostPerToken:    6.0 / 1_000_000,
	},
}

// ModelPricing holds per-token costs for a model.
type ModelPricing struct {
	InputCostPerToken     float64 `json:"input_cost_per_token"`
	OutputCostPerToken    float64 `json:"output_cost_per_token"`
	CacheReadCostPerToken float64 `json:"cache_read_input_token_cost,omitempty"`
}

// PricingCache manages the cached LiteLLM pricing data.
type PricingCache struct {
	dir  string
	mu   sync.RWMutex
	data map[string]json.RawMessage
	done chan struct{}
}

// NewPricingCache creates a new pricing cache, fetches immediately, and starts daily refresh.
func NewPricingCache(statsDir string) *PricingCache {
	pc := newPricingCache(statsDir)
	// Fetch fresh data in background
	go func() {
		pc.fetch()
		pc.scheduleDaily()
	}()
	return pc
}

func newPricingCache(statsDir string) *PricingCache {
	pc := &PricingCache{
		dir:  statsDir,
		data: make(map[string]json.RawMessage),
		done: make(chan struct{}),
	}
	applyBuiltinPricing(pc.data)
	// Load existing cache from disk first
	pc.loadFromDisk()
	return pc
}

func (pc *PricingCache) Close() {
	close(pc.done)
}

func (pc *PricingCache) cachePath() string {
	return filepath.Join(pc.dir, pricingCacheFile)
}

func (pc *PricingCache) loadFromDisk() {
	data, err := os.ReadFile(pc.cachePath())
	if err != nil {
		return
	}
	var parsed map[string]json.RawMessage
	if json.Unmarshal(data, &parsed) == nil {
		applyBuiltinPricing(parsed)
		pc.mu.Lock()
		pc.data = parsed
		pc.mu.Unlock()
		slog.Info("loaded pricing cache from disk", "models", len(parsed))
	}
}

func (pc *PricingCache) fetch() {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(litellmPricingURL)
	if err != nil {
		slog.Warn("failed to fetch LiteLLM pricing", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("failed to fetch LiteLLM pricing", "status", resp.StatusCode)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn("failed to read LiteLLM pricing response", "error", err)
		return
	}

	// Validate JSON
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		slog.Warn("invalid LiteLLM pricing JSON", "error", err)
		return
	}
	applyBuiltinPricing(parsed)

	// Write to disk
	if err := os.MkdirAll(pc.dir, 0o755); err != nil {
		slog.Warn("failed to create pricing cache dir", "error", err)
		return
	}
	if err := os.WriteFile(pc.cachePath(), body, 0o644); err != nil {
		slog.Warn("failed to write pricing cache", "error", err)
		return
	}

	pc.mu.Lock()
	pc.data = parsed
	pc.mu.Unlock()
	slog.Info("updated pricing cache", "models", len(parsed))
}

func applyBuiltinPricing(data map[string]json.RawMessage) {
	for model, pricing := range builtinPricing {
		entry := make(map[string]any)
		if raw, ok := data[model]; ok {
			_ = json.Unmarshal(raw, &entry)
		}
		entry["input_cost_per_token"] = pricing.InputCostPerToken
		entry["cache_read_input_token_cost"] = pricing.CacheReadCostPerToken
		entry["output_cost_per_token"] = pricing.OutputCostPerToken
		if raw, err := json.Marshal(entry); err == nil {
			data[model] = raw
		}
	}
}

func (pc *PricingCache) scheduleDaily() {
	for {
		// Calculate time until next 3:00 AM UTC
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, time.UTC)
		if now.After(next) {
			next = next.Add(24 * time.Hour)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-timer.C:
			pc.fetch()
		case <-pc.done:
			timer.Stop()
			return
		}
	}
}

// GetRawJSON returns the cached pricing JSON bytes for serving via API.
func (pc *PricingCache) GetRawJSON() ([]byte, error) {
	return os.ReadFile(pc.cachePath())
}

// LookupPrice finds pricing for a model name with fuzzy matching.
func (pc *PricingCache) LookupPrice(model string) *ModelPricing {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	return pc.lookupPriceLocked(model)
}

// LookupPrices returns pricing for multiple models. Returns a map of model -> pricing info.
func (pc *PricingCache) LookupPrices(models []string) map[string]json.RawMessage {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	result := make(map[string]json.RawMessage)
	if pc.data == nil {
		return result
	}

	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		// Find the matching key and return its raw JSON
		if key := pc.findKeyLocked(model); key != "" {
			result[model] = pc.data[key]
		}
	}
	return result
}

// findKeyLocked finds the cache key matching a model name. Must hold RLock.
func (pc *PricingCache) findKeyLocked(model string) string {
	candidates := pc.buildCandidates(model)

	// Exact match first
	for _, c := range candidates {
		if _, ok := pc.data[c]; ok {
			return c
		}
	}

	// Prefix match
	for _, c := range candidates {
		for key := range pc.data {
			if strings.HasPrefix(key, c) {
				return key
			}
		}
	}
	return ""
}

// buildCandidates generates model name variations for fuzzy matching.
func (pc *PricingCache) buildCandidates(model string) []string {
	candidates := []string{model}
	normalized := strings.ReplaceAll(model, ".", "-")
	if normalized != model {
		candidates = append(candidates, normalized)
	}
	for _, m := range []string{model, normalized} {
		candidates = append(candidates, "anthropic/"+m, "openai/"+m)
	}
	if strings.HasSuffix(model, "-1m") {
		base := strings.TrimSuffix(model, "-1m")
		baseNorm := strings.ReplaceAll(base, ".", "-")
		candidates = append(candidates, base, baseNorm, "anthropic/"+base, "anthropic/"+baseNorm, "openai/"+base, "openai/"+baseNorm)
	}
	return candidates
}

// lookupPriceLocked finds pricing for a model. Must hold RLock.
func (pc *PricingCache) lookupPriceLocked(model string) *ModelPricing {
	if pc.data == nil {
		return nil
	}

	candidates := pc.buildCandidates(model)

	for _, c := range candidates {
		if raw, ok := pc.data[c]; ok {
			var p ModelPricing
			if json.Unmarshal(raw, &p) == nil && (p.InputCostPerToken > 0 || p.OutputCostPerToken > 0) {
				return &p
			}
		}
	}

	// Prefix match
	for _, c := range candidates {
		for key, raw := range pc.data {
			if strings.HasPrefix(key, c) {
				var p ModelPricing
				if json.Unmarshal(raw, &p) == nil && (p.InputCostPerToken > 0 || p.OutputCostPerToken > 0) {
					return &p
				}
			}
		}
	}

	return nil
}
