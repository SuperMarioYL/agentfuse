package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// responsesStreamEvent is the union of Responses-API SSE frame shapes we care
// about. The Responses API (POST /v1/responses, used by the OpenAI Codex CLI
// for gpt-5/o3) differs from Chat Completions in two load-bearing ways:
//
//   - Text deltas arrive as a top-level `delta` STRING field on
//     `response.output_text.delta` frames (not under
//     choices[].delta.content like Chat Completions).
//   - Final usage arrives on the `response.completed` frame under
//     `response.usage.{input_tokens,output_tokens}` (not a top-level `usage`
//     block on the final frame).
//
// parseDeepSeekUsage (the Chat-Completions walker reused by the openai handler)
// reads neither, so a streamed /v1/responses request yielded in=0, out=0,
// completionText="" and fell back to billing 100 output tokens — fail-open.
// This parser mirrors parseAnthropicUsage's per-event accumulation.
type responsesStreamEvent struct {
	Type string `json:"type"`
	// Delta is a string on response.output_text.delta frames. Other event
	// types carry an object here (or omit it), so we decode lazily below.
	Delta json.RawMessage `json:"delta"`
	// Response is present on response.created / response.completed frames and
	// carries the model + the final usage block.
	Response *struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"response"`
}

// responsesUnary is the non-streamed /v1/responses JSON object: usage is
// top-level and the assistant text lives under output[].content[].text.
type responsesUnary struct {
	Model string `json:"model"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

// parseOpenAIResponsesUsage returns (inputTokens, outputTokens,
// completionText, model). It handles BOTH a unary Responses JSON object and a
// Responses SSE event-stream body. For the stream path it walks every
// `data: { ... }` line, accumulates the `delta` string from
// response.output_text.delta frames, and takes the last non-zero
// input/output token counts it sees (response.created carries
// input_tokens; response.completed carries the final in/out).
func parseOpenAIResponsesUsage(body []byte) (int, int, string, string) {
	// Unary path: a single Responses JSON object.
	var unary responsesUnary
	if err := json.Unmarshal(body, &unary); err == nil &&
		((unary.Usage != nil && (unary.Usage.InputTokens > 0 || unary.Usage.OutputTokens > 0)) ||
			unary.Model != "" || len(unary.Output) > 0) {
		inTok, outTok := 0, 0
		if unary.Usage != nil {
			inTok = unary.Usage.InputTokens
			outTok = unary.Usage.OutputTokens
		}
		if inTok > 0 || outTok > 0 || unary.Model != "" {
			return inTok, outTok, responsesOutputText(unary.Output), unary.Model
		}
	}

	// Stream path.
	var (
		inTok, outTok int
		text          strings.Builder
		model          string
	)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		line = bytes.TrimPrefix(line, []byte("data:"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) || line[0] != '{' {
			continue
		}
		var ev responsesStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		// Accumulate the delta string from text-delta frames. `delta` is a
		// JSON string on response.output_text.delta; on other event types it
		// is either absent or an object, which fails to decode as a string
		// and is skipped — exactly the filter we want.
		if len(ev.Delta) > 0 {
			var s string
			if err := json.Unmarshal(ev.Delta, &s); err == nil && s != "" {
				text.WriteString(s)
			}
		}
		if ev.Response != nil {
			if ev.Response.Model != "" {
				model = ev.Response.Model
			}
			if ev.Response.Usage != nil {
				if ev.Response.Usage.InputTokens > 0 {
					inTok = ev.Response.Usage.InputTokens
				}
				if ev.Response.Usage.OutputTokens > 0 {
					outTok = ev.Response.Usage.OutputTokens
				}
			}
		}
	}
	return inTok, outTok, text.String(), model
}

// responsesOutputText flattens a unary Responses response's output items into
// one string so the accuracy harness can EstimateCompletion on the real
// completion text instead of "".
func responsesOutputText(items []struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}) string {
	var b strings.Builder
	for _, item := range items {
		for _, c := range item.Content {
			if c.Type == "output_text" || c.Type == "text" || c.Type == "" {
				b.WriteString(c.Text)
			}
		}
	}
	return b.String()
}

// isOpenAIResponsesPath reports whether the (prefix-stripped) request path
// targets the Responses API, so the openai handler can route parsing through
// parseOpenAIResponsesUsage instead of the Chat-Completions walker.
func isOpenAIResponsesPath(p string) bool {
	return p == "/v1/responses" || strings.HasPrefix(p, "/v1/responses/")
}
