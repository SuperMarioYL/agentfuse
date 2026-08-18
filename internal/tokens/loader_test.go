package tokens

import (
	"testing"
	"time"

	"github.com/pkoukk/tiktoken-go"
)

// TestGetEncOfflineDoesNotBlock is the v0.11 regression for
// fix-tiktoken-getencoding-network-call: getEnc must NOT do an unbounded
// http.Get against openaipublic.blob.core.windows.net (the default tiktoken-go
// loader does, with http.DefaultClient.Timeout=0). With the embedded offline
// loader registered at init(), getEnc returns in bounded time and yields a
// working encoder. The 15s ceiling is generous; the real cost is a one-time
// parse of the embedded vocab (~200k lines), then tiktoken-go caches it.
func TestGetEncOfflineDoesNotBlock(t *testing.T) {
	done := make(chan struct{})
	var (
		enc *tiktoken.Tiktoken
		err error
	)
	go func() {
		enc, err = getEnc("gpt-4o") // -> o200k_base, served from embed
		close(done)
	}()
	select {
	case <-done:
		if err != nil {
			t.Fatalf("getEnc returned error in offline mode: %v (embed missing?)", err)
		}
		if enc == nil {
			t.Fatal("getEnc returned nil encoder")
		}
		// Sanity: the encoder actually tokenizes a known string.
		if got := len(enc.Encode("Hello, world!", nil, nil)); got == 0 {
			t.Fatal("encoder produced 0 tokens for non-empty input")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("getEnc blocked >15s — unbounded network http.Get still present (v0.6 budget-starvation defect reopened)")
	}
}

// TestOfflineLoaderRejectsUnknownEncoding proves the loader returns an error
// for an encoding we don't ship (p50k_base) instead of falling through to an
// http.Get. encodingFor() never selects p50k_base, but guarding here keeps a
// future change from silently re-opening the network path for an un-embedded
// vocab.
func TestOfflineLoaderRejectsUnknownEncoding(t *testing.T) {
	_, err := tiktoken.GetEncoding("p50k_base")
	if err == nil {
		t.Fatal("expected error for un-embedded encoding p50k_base; the offline loader would have hit the network")
	}
}

// TestEstimateStillWorksWithEmbeddedEncoder re-asserts the existing estimator
// contract after the offline-loader swap: a representative model tokenizes to
// a sane (non-zero, plausible) count using the embedded vocab.
func TestEstimateStillWorksWithEmbeddedEncoder(t *testing.T) {
	got := EstimatePrompt("gpt-4o", "Hello, world!")
	if got < 1 || got > 20 {
		t.Fatalf("EstimatePrompt out of plausible range: %d (embedded encoder broken?)", got)
	}
	// Claude/non-OpenAI path also routes to o200k_base via the embed.
	if got := EstimatePrompt("claude-3-5-sonnet-20241022", "Hello, world!"); got < 1 {
		t.Fatalf("Claude-named model should still tokenize via embedded o200k: %d", got)
	}
}

// TestParseBpeRanksMirrorsDefaultLoader guards the offline parser against the
// default loader's expected shape: base64 token + rank per line, ranks must be
// 0-indexed and contiguous for a real OpenAI vocab.
func TestParseBpeRanksMirrorsDefaultLoader(t *testing.T) {
	ranks, err := parseBpeRanks([]byte("IQ== 0\nIg== 1\nIw== 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ranks) != 3 {
		t.Fatalf("expected 3 ranks, got %d", len(ranks))
	}
	for i := 0; i < 3; i++ {
		if ranks[string([]byte{'!' + byte(i)})] != i {
			t.Fatalf("rank for %q = %d, want %d", string([]byte{'!' + byte(i)}), ranks[string([]byte{'!' + byte(i)})], i)
		}
	}
	// Blank trailing line is tolerated (the embedded files end with \n).
	if _, err := parseBpeRanks([]byte("IQ== 0\n\n")); err != nil {
		t.Fatalf("trailing blank line should be tolerated: %v", err)
	}
	// Malformed rank line is skipped (SplitN len check), not panicked.
	ranks, err = parseBpeRanks([]byte("IQ== 0\nbogus\nIg== 1\n"))
	if err != nil {
		t.Fatalf("malformed line should be skipped, not error: %v", err)
	}
	if ranks[string([]byte{'"'})] != 1 {
		t.Fatalf("expected rank 1 for \", got %d", ranks[string([]byte{'"'})])
	}
}
