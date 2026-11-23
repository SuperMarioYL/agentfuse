package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
)

// v0.16: fix-anthropic-cache-tokens-dropped.
//
// Anthropic's Messages usage block reports cache_creation_input_tokens and
// cache_read_input_tokens as counters SEPARATE from input_tokens (input_tokens
// excludes cached prompt tokens). The handler declared only input/output, so
// every prompt-cached turn — the Claude Code default — recorded a fraction of its
// real input spend: the body below cost ~$0.0435 but the ledger recorded $0.0015,
// leaving realized spend free to exceed the cap without tripping it.
const (
	anthropicCacheIn         = 10
	anthropicCacheOut        = 100
	anthropicCacheRead       = 90000
	anthropicCacheWrite      = 4000
	anthropicCacheWantTokens = anthropicCacheIn + anthropicCacheRead + anthropicCacheWrite

	// The published Anthropic prompt-cache multipliers (5-minute tier): a cache
	// write costs 1.25x the input rate, a cache read 0.1x.
	cacheWriteMultiplier = 1.25
	cacheReadMultiplier  = 0.1
)

const anthropicCacheUnaryBody = `{"id":"msg_cache","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":100,"cache_creation_input_tokens":4000,"cache_read_input_tokens":90000},"content":[{"type":"text","text":"ok"}]}`

// anthropicCacheSSEBody splits the same usage across the two frames Anthropic
// uses: message_start carries the input side (including both cache counters),
// message_delta carries output_tokens.
const anthropicCacheSSEBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_cache","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":4000,"cache_read_input_tokens":90000}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}}

event: message_stop
data: {"type":"message_stop"}

`

// wantAnthropicCacheUSD is the weighted cost the ledger must record.
func wantAnthropicCacheUSD(inRate, outRate float64) float64 {
	return float64(anthropicCacheIn)*inRate +
		float64(anthropicCacheOut)*outRate +
		float64(anthropicCacheWrite)*cacheWriteMultiplier*inRate +
		float64(anthropicCacheRead)*cacheReadMultiplier*inRate
}

func TestAnthropicCacheTokensBilled(t *testing.T) {
	inRate := budget.CostFromUsageWithProvider("anthropic", "claude-sonnet-4", 1_000_000, 0) / 1_000_000
	outRate := budget.CostFromUsageWithProvider("anthropic", "claude-sonnet-4", 0, 1_000_000) / 1_000_000
	want := wantAnthropicCacheUSD(inRate, outRate)
	// The broken path billed input_tokens + output_tokens only; the cache terms
	// dominate (90k cached read + 4k write) so the two figures are far apart.
	broken := float64(anthropicCacheIn)*inRate + float64(anthropicCacheOut)*outRate

	cases := []struct {
		name string
		body []byte
		ct   string
	}{
		{"unary", []byte(anthropicCacheUnaryBody), "application/json"},
		{"stream", []byte(anthropicCacheSSEBody), "text/event-stream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.ct)
				_, _ = w.Write(c.body)
			}))
			defer upstream.Close()

			prev := AnthropicUpstream
			AnthropicUpstream = upstream.URL
			defer func() { AnthropicUpstream = prev }()

			led := mustOpenLedger(t)
			cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
			s := New("/proj/cache-"+c.name, cfg, led, &account.File{Accounts: map[string]account.Account{}})
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = s.Stop(ctx)
			}()

			reqBody := []byte(`{"model":"claude-sonnet-4","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)
			req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(reqBody))
			req.Header.Set("x-api-key", "sk-ant-test-1234567890")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}

			got, err := led.ProjectTotal("/proj/cache-" + c.name)
			if err != nil {
				t.Fatal(err)
			}
			if got.TokensIn != anthropicCacheWantTokens {
				t.Errorf("TokensIn = %d, want %d (input + cache write + cache read) — the cache counters were dropped",
					got.TokensIn, anthropicCacheWantTokens)
			}
			if got.TokensOut != anthropicCacheOut {
				t.Errorf("TokensOut = %d, want %d", got.TokensOut, anthropicCacheOut)
			}
			// Relative tolerance: the ledger sums floats.
			if diff := got.USD - want; diff > want*0.001 || diff < -want*0.001 {
				t.Errorf("USD = %.6f, want %.6f (input-only figure is %.6f) — cache tokens must be priced at %vx (write) / %vx (read) the input rate",
					got.USD, want, broken, cacheWriteMultiplier, cacheReadMultiplier)
			}
		})
	}
}
