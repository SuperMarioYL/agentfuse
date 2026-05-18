package tokens

import "testing"

func TestEstimatePromptReturnsPositive(t *testing.T) {
	// Known canonical short input; exact count depends on encoding but must be
	// in a reasonable range and never zero.
	got := EstimatePrompt("gpt-4o", "Hello, world!")
	if got < 1 || got > 20 {
		t.Fatalf("EstimatePrompt out of plausible range: %d", got)
	}
}

func TestEstimatePromptEmptyIsZero(t *testing.T) {
	if got := EstimatePrompt("gpt-4o", ""); got != 0 {
		t.Fatalf("empty prompt should be 0 tokens, got %d", got)
	}
}

func TestCompletionRoundsUpToHundred(t *testing.T) {
	// Any non-empty input should round to at least 100.
	got := EstimateCompletion("gpt-4o", "ok")
	if got != CompletionRoundUp {
		t.Fatalf("tiny completion should round to %d, got %d", CompletionRoundUp, got)
	}
}

func TestCompletionEmptyStillRoundsToStep(t *testing.T) {
	// Even an "empty" stream counts as one unit of agent activity — under-billing
	// is the cardinal sin.
	got := EstimateCompletion("gpt-4o", "")
	if got != CompletionRoundUp {
		t.Fatalf("empty completion should round to %d (never 0), got %d",
			CompletionRoundUp, got)
	}
}

func TestCompletionLongerRoundsUp(t *testing.T) {
	// Build a string definitely > 100 tokens.
	long := ""
	for i := 0; i < 600; i++ {
		long += "word "
	}
	got := EstimateCompletion("gpt-4o", long)
	if got%CompletionRoundUp != 0 {
		t.Fatalf("must round to a multiple of %d, got %d", CompletionRoundUp, got)
	}
	if got < 200 {
		t.Fatalf("expected > 200 tokens for a 600-word completion, got %d", got)
	}
}

func TestEncodingForClaudeUsesO200k(t *testing.T) {
	// Smoke test that an Anthropic/non-OpenAI model picks a usable encoding.
	got := EstimatePrompt("claude-3-5-sonnet-20241022", "Hello, world!")
	if got < 1 {
		t.Fatalf("Claude-named model should still tokenize: %d", got)
	}
}

func TestRoundUpHelper(t *testing.T) {
	cases := []struct{ in, step, want int }{
		{0, 100, 100},
		{1, 100, 100},
		{99, 100, 100},
		{100, 100, 100},
		{101, 100, 200},
		{250, 100, 300},
	}
	for _, c := range cases {
		if got := roundUp(c.in, c.step); got != c.want {
			t.Errorf("roundUp(%d,%d)=%d want %d", c.in, c.step, got, c.want)
		}
	}
}
