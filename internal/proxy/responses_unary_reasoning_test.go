package proxy

import (
	"strings"
	"testing"
)

// TestParseOpenAIResponsesUsage_UnaryReasoning is the v0.13 regression for
// fix-openai-responses-unary-reasoning-not-captured. The v0.12.0
// fix-anthropic-thinking-delta-not-captured milestone closed the Anthropic
// reasoning gap in both the stream and unary paths and asserted "the OpenAI
// Responses parser is NOT affected — its Delta is a json.RawMessage that
// accumulates any string delta, including reasoning." That assertion holds for
// the STREAM path (response.reasoning_summary_text.delta frames carry `delta`
// as a top-level string the walker accumulates), but is FALSE for the UNARY
// path: a unary /v1/responses reasoning-model (o3/gpt-5) body carries its
// reasoning trace under output[].summary[] (item type "reasoning", content
// type "summary_text"), NOT under output[].content[], which responsesOutputText
// alone read. So a unary o3 response recorded EstimateCompletion(final-answer
// -only) against output_tokens (which include the reasoning tokens) —
// structurally low, false-triggering the §8 >25% kill criterion the v0.4/v0.5/
// v0.12 harness fixes were built to make honestly measurable. Billing was
// unaffected (CostFromUsageWithProvider uses upstream output_tokens).
//
// After the fix responsesOutputText also reads summary_text entries from
// reasoning items, so the captured completion text includes the reasoning
// summary, not just the final answer.
func TestParseOpenAIResponsesUsage_UnaryReasoning(t *testing.T) {
	// Realistic unary /v1/responses body for a reasoning model (o3). The
	// reasoning trace lives under output[].summary[] (type "reasoning" /
	// "summary_text"); usage.output_tokens INCLUDES the reasoning tokens
	// (output_tokens_details.reasoning_tokens).
	body := []byte(`{
		"id":"resp_01","model":"o3","status":"completed",
		"usage":{"input_tokens":120,"output_tokens":540,"output_tokens_details":{"reasoning_tokens":480}},
		"output":[
			{"type":"reasoning","id":"rs_0","summary":[{"type":"summary_text","text":"Let me think step by step. First, consider the constraints."}]},
			{"type":"message","id":"msg_0","content":[{"type":"output_text","text":"The answer is 42."}]}
		]
	}`)
	in, out, text, model := parseOpenAIResponsesUsage(body)

	if model != "o3" {
		t.Fatalf("unary model wrong: %q want o3", model)
	}
	if in != 120 {
		t.Fatalf("unary input tokens wrong: got %d want 120", in)
	}
	if out != 540 {
		t.Fatalf("unary output tokens wrong: got %d want 540 (output_tokens incl. reasoning)", out)
	}

	// The defect: completionText was just "The answer is 42." (the final
	// answer), excluding the reasoning summary that output_tokens bills.
	// After the fix the reasoning summary text must be present too.
	if !strings.Contains(text, "Let me think step by step") {
		t.Fatalf("unary reasoning summary NOT captured in completionText: got %q\n"+
			"  (responsesOutputText read only output[].content[], missing the\n"+
			"  output[].summary[] reasoning trace billed as output_tokens —\n"+
			"  EstimateCompletion would be structurally low vs outTok, false-\n"+
			"  triggering the §8 >25%% kill criterion)", text)
	}
	if !strings.Contains(text, "The answer is 42.") {
		t.Fatalf("unary final-answer text NOT captured in completionText: got %q", text)
	}

	// Sanity: the reasoning summary is substantial, so the captured text is
	// materially longer than the final answer alone — the accuracy harness
	// now measures an honest estimate instead of roundUp(~4,100)=100.
	if len(text) <= len("The answer is 42.") {
		t.Fatalf("unary completionText not extended by the reasoning summary: len=%d", len(text))
	}
}
