package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Price is USD per million tokens.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// defaultPrices are keyed by model id prefix; the longest matching prefix
// wins, so "claude-sonnet-4-5-20250929" matches "claude-sonnet-4-5".
//
// Prices change. Anthropic rows are from
// https://platform.claude.com/docs/en/about-claude/pricing (checked Oct 2026).
// OpenAI rows are from a third-party summary (May 2026) and should be checked
// against https://openai.com/api/pricing. Override any of them, or add models,
// in prices.json in the data directory.
var defaultPrices = map[string]Price{
	"claude-fable-5-1":  {10, 50, 0.25, 12.50},
	"claude-mythos-5-1": {10, 50, 0.25, 12.50},
	"claude-fable-5":    {10, 50, 1, 12.50},
	"claude-mythos-5":   {10, 50, 1, 12.50},
	"claude-opus-5-5":   {4, 20, 0.20, 5},
	"claude-opus-5":     {5, 25, 0.50, 6.25},
	"claude-opus-4-8":   {5, 25, 0.50, 6.25},
	"claude-opus-4-7":   {5, 25, 0.50, 6.25},
	"claude-opus-4-6":   {5, 25, 0.50, 6.25},
	"claude-opus-4-5":   {5, 25, 0.50, 6.25},
	"claude-opus-4-1":   {15, 75, 1.50, 18.75},
	"claude-opus-4":     {15, 75, 1.50, 18.75},
	"claude-sonnet-5-5": {2, 10, 0.10, 2.50},
	"claude-sonnet-5":   {2, 10, 0.20, 2.50},
	"claude-sonnet-4-6": {3, 15, 0.30, 3.75},
	"claude-sonnet-4-5": {3, 15, 0.30, 3.75},
	"claude-sonnet-4":   {3, 15, 0.30, 3.75},
	"claude-haiku-5-5":  {0.10, 0.50, 0.01, 0.125}, // prompts <=100K; see haikuLongPrompt
	"claude-haiku-4-5":  {1, 5, 0.10, 1.25},
	"claude-3-5-haiku":  {0.80, 4, 0.08, 1},

	"gpt-5.6-sol":   {5, 30, 0.50, 0},
	"gpt-5.6-terra": {2.50, 15, 0.25, 0},
	"gpt-5.6-luna":  {1, 6, 0.10, 0},
	"gpt-5.5":       {5, 30, 0.50, 0},
	"gpt-5.5-pro":   {30, 180, 30, 0},
	"gpt-5.4":       {2.50, 15, 0.25, 0},
	"gpt-5.4-mini":  {0.75, 4.50, 0.075, 0},
	"gpt-5.4-nano":  {0.20, 1.25, 0.02, 0},
	"gpt-5.4-pro":   {30, 180, 30, 0},
	"gpt-5.3-codex": {1.75, 14, 0.175, 0},
}

// Claude Haiku 5.5 bills prompts over 100K tokens at a higher tier.
var haikuLongPrompt = Price{0.50, 2.50, 0.05, 0.625}

type Pricer struct{ prices map[string]Price }

// LoadPricer merges prices.json (if present) over the defaults.
func LoadPricer(path string) (*Pricer, error) {
	p := &Pricer{prices: map[string]Price{}}
	for k, v := range defaultPrices {
		p.prices[k] = v
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	var overrides map[string]Price
	if err := json.Unmarshal(data, &overrides); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range overrides {
		p.prices[k] = v
	}
	return p, nil
}

func (p *Pricer) lookup(model string) (string, Price, bool) {
	best := ""
	for k := range p.prices {
		if strings.HasPrefix(model, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return "", Price{}, false
	}
	return best, p.prices[best], true
}

// Cost returns the USD cost of a call, or nil if the model has no price.
func (p *Pricer) Cost(model string, u Usage) *float64 {
	key, pr, ok := p.lookup(model)
	if !ok {
		return nil
	}
	// Apply the long-prompt tier unless the user overrode Haiku 5.5's price.
	if key == "claude-haiku-5-5" && pr == defaultPrices[key] && u.Input+u.CacheRead+u.CacheWrite > 100_000 {
		pr = haikuLongPrompt
	}
	c := (float64(u.Input)*pr.Input + float64(u.Output)*pr.Output +
		float64(u.CacheRead)*pr.CacheRead + float64(u.CacheWrite)*pr.CacheWrite) / 1e6
	return &c
}
