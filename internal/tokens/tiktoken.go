// Package tokens wraps github.com/pkoukk/tiktoken-go to give the proxy a local
// fallback when an upstream's streaming response omits the `usage` block. The
// estimator is deliberately conservative: completion counts are rounded UP to
// the nearest 100 tokens, because under-billing the user is unacceptable. A
// 10-15% over-estimate is fine — the kill-switch is supposed to err on the
// side of refusing one extra request.
//
// The package keeps zero global state besides the encoding cache. v0.11
// (fix-tiktoken-getencoding-network-call): tiktoken-go's default BpeLoader
// does an http.Get against openaipublic.blob.core.windows.net when its on-disk
// cache is absent, and http.DefaultClient has Timeout=0 — an unbounded block
// that re-opens the v0.6 budget-starvation defect while a ledger reservation
// is held. init() in loader.go swaps in an offlineBpeLoader that serves the
// two embedded vocab files (cl100k_base, o200k_base), so the per-request path
// never opens a network connection. An encoding we don't ship errors out and
// the estimator falls back to chars/4.
package tokens

import (
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

// CompletionRoundUp is the granularity we ceil completion-token counts to.
// Public so tests can assert against it.
const CompletionRoundUp = 100

var (
	cacheMu sync.RWMutex
	encs    = map[string]*tiktoken.Tiktoken{}
)

// encodingFor picks a tiktoken encoding name for the given model. tiktoken-go
// understands o200k_base (gpt-4o family), cl100k_base (gpt-3.5/gpt-4),
// p50k_base, and r50k_base. For non-OpenAI providers (Claude, Gemini, DeepSeek,
// Llama, …) we use o200k_base — it's the closest BPE family in active use and
// the estimator only needs to be conservative, not exact.
func encodingFor(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "gpt-4o"),
		strings.HasPrefix(m, "gpt-5"),
		strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "gpt-4.1"):
		return "o200k_base"
	case strings.HasPrefix(m, "gpt-3.5"),
		strings.HasPrefix(m, "gpt-4"):
		return "cl100k_base"
	default:
		// Claude, Gemini, DeepSeek, Llama, Mistral, Grok, Qwen — all served by
		// o200k_base as a conservative default.
		return "o200k_base"
	}
}

func getEnc(model string) (*tiktoken.Tiktoken, error) {
	name := encodingFor(model)
	cacheMu.RLock()
	if e, ok := encs[name]; ok {
		cacheMu.RUnlock()
		return e, nil
	}
	cacheMu.RUnlock()

	enc, err := tiktoken.GetEncoding(name)
	if err != nil {
		// Either an un-embedded encoding (offlineBpeLoader refused to touch the
		// network) or a parse failure on the embedded vocab — callers fall back
		// to chars/4 via fallbackCharsPerToken. The unbounded http.Get defect is
		// gone: this path returns in bounded time.
		return nil, err
	}
	cacheMu.Lock()
	encs[name] = enc
	cacheMu.Unlock()
	return enc, nil
}

// EstimatePrompt returns the tiktoken count of prompt for the given model.
// Errors during encoding fall back to a character/4 estimate so the proxy
// never panics on an exotic input.
func EstimatePrompt(model, prompt string) int {
	enc, err := getEnc(model)
	if err != nil {
		return fallbackCharsPerToken(prompt)
	}
	return len(enc.Encode(prompt, nil, nil))
}

// EstimateCompletion returns the tiktoken count of completion, rounded UP to
// the nearest CompletionRoundUp tokens. Under-billing is not acceptable for
// the kill-switch; over-billing by a few percent is.
func EstimateCompletion(model, completion string) int {
	enc, err := getEnc(model)
	var raw int
	if err != nil {
		raw = fallbackCharsPerToken(completion)
	} else {
		raw = len(enc.Encode(completion, nil, nil))
	}
	return roundUp(raw, CompletionRoundUp)
}

func roundUp(n, step int) int {
	if step <= 0 {
		return n
	}
	if n <= 0 {
		// Even an empty completion that the agent surfaced is at least 1
		// "thinking" token in the user's eyes — round to one step.
		return step
	}
	rem := n % step
	if rem == 0 {
		return n
	}
	return n + (step - rem)
}

func fallbackCharsPerToken(s string) int {
	// 4 chars/token is the well-known OpenAI rule-of-thumb; for unknown
	// encodings it is "wrong but conservative enough".
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}
