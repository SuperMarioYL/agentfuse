package budget

import (
	"fmt"
	"strings"
	"sync"

	"github.com/SuperMarioYL/agentfuse/internal/prices"
)

// Decision is the result of asking the enforcer whether a request may proceed.
type Decision struct {
	Allow        bool
	Reason       string
	SuggestedCmd string
	CurrentUSD   float64
	CapUSD       float64
	EstimateUSD  float64
}

// Price is dollars per 1M tokens (input, output).
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
}

// Prices is a small static table. We err high on completion so the pre-flight
// estimate is conservative — the kill-switch should deny rather than slip.
var Prices = map[string]Price{
	// Anthropic
	"claude-3-5-sonnet":   {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-3-5-haiku":    {InputPer1M: 0.80, OutputPer1M: 4.00},
	"claude-3-7-sonnet":   {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-sonnet-4":     {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-sonnet-4-5":   {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-opus-4":       {InputPer1M: 15.00, OutputPer1M: 75.00},
	"claude-opus-4-1":     {InputPer1M: 15.00, OutputPer1M: 75.00},
	"claude-haiku-4-5":    {InputPer1M: 1.00, OutputPer1M: 5.00},
	// OpenAI
	"gpt-4o":          {InputPer1M: 2.50, OutputPer1M: 10.00},
	"gpt-4o-mini":     {InputPer1M: 0.15, OutputPer1M: 0.60},
	"gpt-4-turbo":     {InputPer1M: 10.00, OutputPer1M: 30.00},
	"gpt-4.1":         {InputPer1M: 2.00, OutputPer1M: 8.00},
	"gpt-5":           {InputPer1M: 5.00, OutputPer1M: 15.00},
	"o3":              {InputPer1M: 2.00, OutputPer1M: 8.00},
	"o3-mini":         {InputPer1M: 1.10, OutputPer1M: 4.40},
	"gpt-3.5-turbo":   {InputPer1M: 0.50, OutputPer1M: 1.50},
}

// fallback used when model name is unknown — picks a mid-tier price so we err
// closer to deny than allow.
var fallbackPrice = Price{InputPer1M: 5.00, OutputPer1M: 20.00}

// PriceFor returns the price entry for the longest matching prefix of model.
func PriceFor(model string) Price {
	m := strings.ToLower(model)
	var best string
	for k := range Prices {
		if strings.HasPrefix(m, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return fallbackPrice
	}
	return Prices[best]
}

// EstimateRequest returns a conservative USD estimate for a request given the
// model and the prompt token count reported by the client (if known). When
// promptTokens is unknown (0) we use a floor that still moves the needle for
// pathological loops.
func EstimateRequest(model string, promptTokens int, maxOutputTokens int) float64 {
	p := PriceFor(model)
	if promptTokens <= 0 {
		promptTokens = 1000
	}
	if maxOutputTokens <= 0 {
		// Assume completion ≈ prompt (conservative for kill-switch).
		maxOutputTokens = promptTokens
	}
	usd := float64(promptTokens)*p.InputPer1M/1_000_000 +
		float64(maxOutputTokens)*p.OutputPer1M/1_000_000
	return usd
}

// CostFromUsage uses the post-response usage block to compute exact dollars.
//
// v0.2.0 NOTE: this entry point is kept for backwards compatibility with the
// v0.1 Anthropic + OpenAI handlers, which still call it with provider implicit
// in the model name. New v0.2 handlers (gemini, deepseek, openai_compat)
// should call CostFromUsageWithProvider so the externalized prices.Table is
// consulted instead of the static map above.
func CostFromUsage(model string, inputTokens, outputTokens int) float64 {
	p := PriceFor(model)
	return float64(inputTokens)*p.InputPer1M/1_000_000 +
		float64(outputTokens)*p.OutputPer1M/1_000_000
}

// ---------------- v0.2.0: externalized price table ----------------

var (
	priceTableMu   sync.RWMutex
	priceTable     *prices.Table
	priceTableErr  error
	priceTableOnce sync.Once
)

// PriceTable returns the lazily-loaded merged price table (bundled + user
// ~/.fuse/prices.toml override). The same Table is reused for the lifetime of
// the process — no auto-refresh, no network. Errors are sticky.
func PriceTable() (*prices.Table, error) {
	priceTableOnce.Do(func() {
		t, err := prices.LoadDefault()
		priceTableMu.Lock()
		priceTable = t
		priceTableErr = err
		priceTableMu.Unlock()
	})
	priceTableMu.RLock()
	defer priceTableMu.RUnlock()
	return priceTable, priceTableErr
}

// SetPriceTable lets tests inject a pre-built table.
func SetPriceTable(t *prices.Table) {
	priceTableMu.Lock()
	defer priceTableMu.Unlock()
	priceTable = t
	priceTableErr = nil
	// Mark sync.Once as done so subsequent PriceTable() calls return our value.
	priceTableOnce.Do(func() {})
}

// CostFromUsageWithProvider computes USD using the externalized price table
// (bundled + user override). Falls back gracefully to the v0.1 static prices
// when the table can't be loaded — the kill-switch must never panic during
// cost compute.
func CostFromUsageWithProvider(provider, model string, inputTokens, outputTokens int) float64 {
	t, err := PriceTable()
	if err != nil || t == nil {
		return CostFromUsage(model, inputTokens, outputTokens)
	}
	return t.Cost(provider, model, inputTokens, outputTokens)
}

// EstimateRequestWithProvider mirrors EstimateRequest but consults the
// externalized table.
func EstimateRequestWithProvider(provider, model string, promptTokens, maxOutputTokens int) float64 {
	if promptTokens <= 0 {
		promptTokens = 1000
	}
	if maxOutputTokens <= 0 {
		maxOutputTokens = promptTokens
	}
	t, err := PriceTable()
	if err != nil || t == nil {
		return EstimateRequest(model, promptTokens, maxOutputTokens)
	}
	e, _ := t.Lookup(provider, model)
	return float64(promptTokens)*e.InputUSDPer1K/1000.0 +
		float64(maxOutputTokens)*e.OutputUSDPer1K/1000.0
}

// Decide compares current spend + estimate against cap.
func Decide(currentUSD, capUSD, estimateUSD float64, projectRoot string) Decision {
	d := Decision{
		CurrentUSD:  currentUSD,
		CapUSD:      capUSD,
		EstimateUSD: estimateUSD,
	}
	projected := currentUSD + estimateUSD
	if projected > capUSD {
		d.Allow = false
		d.Reason = fmt.Sprintf("budget exceeded for project %s ($%.2f / $%.2f)",
			projectRoot, projected, capUSD)
		d.SuggestedCmd = "fuse cap +5"
		return d
	}
	d.Allow = true
	return d
}
