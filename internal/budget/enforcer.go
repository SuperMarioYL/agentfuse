package budget

import (
	"fmt"
	"strings"
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
func CostFromUsage(model string, inputTokens, outputTokens int) float64 {
	p := PriceFor(model)
	return float64(inputTokens)*p.InputPer1M/1_000_000 +
		float64(outputTokens)*p.OutputPer1M/1_000_000
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
