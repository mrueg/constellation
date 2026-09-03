package embed

import (
	"context"
	"math"
	"testing"
)

// Field weights are fractional — a README term arrives at 0.15 — and the
// sublinear transform 1+log(tf) is negative below 1/e. That put 73% of the
// matrix below zero, so two repositories sharing a rare term were pushed
// apart, and cosine similarity left [0,1], which every threshold downstream
// assumes it stays inside.
func TestWeightsAreNeverNegative(t *testing.T) {
	tf := &TFIDF{MinDF: 1, MaxDFRatio: 1, MaxVocab: 1000}
	for _, w := range []float64{0.01, 0.15, 0.3, 1 / math.E, 0.5, 0.9, 1, 1.2, 3, 18} {
		if got := tf.weightOf(w, 2.0, 1, 1); got < 0 {
			t.Errorf("weightOf(tf=%v) = %v, want >= 0", w, got)
		}
	}
}

// More evidence for a term must never count for less. The old formula was
// monotone but crossed zero, so a term seen once scored below one seen not at
// all.
func TestWeightIsMonotoneInTermFrequency(t *testing.T) {
	tf := &TFIDF{}
	prev := math.Inf(-1)
	for _, w := range []float64{0.05, 0.15, 0.5, 0.99, 1, 1.01, 2, 5, 20} {
		got := tf.weightOf(w, 1.5, 1, 1)
		if got < prev {
			t.Errorf("weightOf(%v) = %v, lower than the previous %v", w, got, prev)
		}
		prev = got
	}
}

// The fix must not disturb the region it was not broken in: at and above a
// weight of one the score is unchanged, so tuning measured against
// descriptions and topics still holds.
func TestWeightUnchangedAtAndAboveOne(t *testing.T) {
	tf := &TFIDF{}
	for _, w := range []float64{1, 1.2, 3, 18} {
		if got, want := tf.weightOf(w, 2.0, 1, 1), (1+math.Log(w))*2.0; got != want {
			t.Errorf("weightOf(%v) = %v, want %v", w, got, want)
		}
	}
	// Continuous at the join.
	if got := tf.weightOf(1, 2.0, 1, 1); got != 2.0 {
		t.Errorf("weightOf(1) = %v, want the idf itself", got)
	}
}

// An embedded corpus must come out non-negative end to end, which is the
// property the clustering and every similarity threshold rely on.
func TestEmbeddedVectorsAreNonNegative(t *testing.T) {
	docs := []Doc{
		{Terms: map[string]float64{"kubernetes": 3, "operator": 1, "telemetry": 0.15}},
		{Terms: map[string]float64{"kubernetes": 3, "controller": 1, "telemetry": 0.15}},
		{Terms: map[string]float64{"rust": 3, "parser": 1, "wasm": 0.15}},
	}
	sp, err := (&TFIDF{MinDF: 1, MaxDFRatio: 1, MaxVocab: 100}).Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range sp.Rows {
		for j, v := range row.Val {
			if v < 0 {
				t.Errorf("row %d term %q has negative weight %v", i, sp.Terms[row.Idx[j]], v)
			}
		}
	}
	// And therefore no pair can have a negative similarity.
	if sim := sp.Rows[0].DotSparse(sp.Rows[2]); sim < 0 {
		t.Errorf("similarity between unrelated documents is negative: %v", sim)
	}
}
