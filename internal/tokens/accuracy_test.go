package tokens

import "testing"

func TestAccuracyRecorderMedianError(t *testing.T) {
	r := NewAccuracyRecorder()
	// Estimates vs actuals: errors +20%, -10%, 0%.
	r.Record("openai", "gpt-4o", 120, 100) // +20%
	r.Record("openai", "gpt-4o", 90, 100)  // -10%
	r.Record("openai", "gpt-4o", 100, 100) // 0%

	st := r.Stats()
	if st.N != 3 {
		t.Fatalf("N=%d want 3", st.N)
	}
	// signed errors sorted: -10, 0, +20 → median 0
	if st.MedianSignedPct != 0 {
		t.Fatalf("median signed=%.2f want 0", st.MedianSignedPct)
	}
	// abs errors sorted: 0, 10, 20 → median 10
	if st.MedianAbsPct != 10 {
		t.Fatalf("median abs=%.2f want 10", st.MedianAbsPct)
	}
	if st.ExceedsThreshold {
		t.Fatal("10% median abs should not exceed the 25% threshold")
	}
}

func TestAccuracyRecorderExceedsThreshold(t *testing.T) {
	r := NewAccuracyRecorder()
	r.Record("anthropic", "claude-sonnet-4", 200, 100) // +100%
	r.Record("anthropic", "claude-sonnet-4", 180, 100) // +80%
	st := r.Stats()
	if !st.ExceedsThreshold {
		t.Fatalf("median abs %.1f%% should exceed %0.f%%", st.MedianAbsPct, AccuracyThresholdPct)
	}
}

func TestAccuracyRecorderSkipsZeroActual(t *testing.T) {
	r := NewAccuracyRecorder()
	r.Record("openai", "gpt-4o", 100, 0) // unusable — actual 0
	st := r.Stats()
	if st.N != 0 {
		t.Fatalf("zero-actual sample must be dropped from stats, N=%d", st.N)
	}
}

func TestRecordSampleIgnoresZeroActual(t *testing.T) {
	// RecordSample on the default recorder must ignore a zero actual (fallback
	// path) so it never pollutes the accuracy stats.
	before := Stats().N
	RecordSample("openai", "gpt-4o", 100, 0)
	if Stats().N != before {
		t.Fatal("RecordSample must ignore zero-actual observations")
	}
}
