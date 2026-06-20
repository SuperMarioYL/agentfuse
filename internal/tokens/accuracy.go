// accuracy.go — measurement-only harness that records, per (provider, model),
// the tiktoken estimate alongside the upstream-reported usage whenever BOTH are
// present on the same response, then computes the median signed and absolute
// percentage error.
//
// This exists to make the v0.2 §8 kill criterion ("if the median error between
// tiktoken-go estimates and upstream-reported usage exceeds 25%, redesign the
// estimator") actually evaluable. It does NOT change the estimator math in
// tiktoken.go — it only observes. The recorder is process-local, zero network,
// and guarded by a mutex so the proxy handlers can call it from any goroutine.
package tokens

import (
	"sort"
	"sync"
)

// Sample is one observation: the tiktoken estimate for a completion vs the
// upstream-reported token count, scoped to a (provider, model).
type Sample struct {
	Provider     string
	Model        string
	EstTokens    int // tiktoken estimate
	ActualTokens int // upstream-reported usage
}

// signedErrPct returns the signed percentage error of the estimate relative to
// the upstream actual: (est-actual)/actual*100. Samples with a zero actual are
// skipped by the caller (division would be undefined / meaningless).
func (s Sample) signedErrPct() float64 {
	if s.ActualTokens == 0 {
		return 0
	}
	return float64(s.EstTokens-s.ActualTokens) / float64(s.ActualTokens) * 100.0
}

// AccuracyStats is the per-call summary the introspection surface prints.
type AccuracyStats struct {
	N                int     // number of usable samples (actual > 0)
	MedianSignedPct  float64 // median of signed error %
	MedianAbsPct     float64 // median of |error| %
	ExceedsThreshold bool    // MedianAbsPct > Threshold
}

// AccuracyThresholdPct is the §8 kill criterion: a median absolute error above
// this means the estimator should be redesigned. Public so callers/tests can
// reference the same number.
const AccuracyThresholdPct = 25.0

// AccuracyRecorder accumulates samples and computes median error on demand.
// The zero value is NOT ready — use NewAccuracyRecorder. A package-level default
// recorder (DefaultRecorder) is provided so handlers can record without
// threading an instance through every call site.
type AccuracyRecorder struct {
	mu      sync.Mutex
	samples []Sample
}

// NewAccuracyRecorder returns an empty recorder.
func NewAccuracyRecorder() *AccuracyRecorder {
	return &AccuracyRecorder{}
}

// DefaultRecorder is the process-wide recorder the proxy handlers write to.
var DefaultRecorder = NewAccuracyRecorder()

// Record appends one observation. A non-positive actual is dropped at compute
// time (it cannot yield a meaningful percentage error), but is still stored so
// the raw count is honest.
func (r *AccuracyRecorder) Record(provider, model string, estTokens, actualTokens int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, Sample{
		Provider:     provider,
		Model:        model,
		EstTokens:    estTokens,
		ActualTokens: actualTokens,
	})
}

// RecordSample records a tiktoken-vs-upstream observation on the default
// recorder. Handlers call this when they have BOTH a local tiktoken estimate
// and an upstream-reported usage for the same completion. Calls with a zero
// actual (upstream omitted usage — i.e. the fallback path was taken) are
// ignored, since there is nothing to compare the estimate against.
func RecordSample(provider, model string, estTokens, actualTokens int) {
	if actualTokens <= 0 {
		return
	}
	DefaultRecorder.Record(provider, model, estTokens, actualTokens)
}

// Stats returns the median signed/absolute percentage error across all usable
// samples (those with actual > 0).
func (r *AccuracyRecorder) Stats() AccuracyStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	signed := make([]float64, 0, len(r.samples))
	abs := make([]float64, 0, len(r.samples))
	for _, s := range r.samples {
		if s.ActualTokens <= 0 {
			continue
		}
		e := s.signedErrPct()
		signed = append(signed, e)
		if e < 0 {
			abs = append(abs, -e)
		} else {
			abs = append(abs, e)
		}
	}

	st := AccuracyStats{N: len(signed)}
	if st.N == 0 {
		return st
	}
	st.MedianSignedPct = median(signed)
	st.MedianAbsPct = median(abs)
	st.ExceedsThreshold = st.MedianAbsPct > AccuracyThresholdPct
	return st
}

// Stats returns the default recorder's stats.
func Stats() AccuracyStats { return DefaultRecorder.Stats() }

// median returns the median of xs. xs is copied before sorting so the caller's
// slice ordering is preserved.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2.0
}
