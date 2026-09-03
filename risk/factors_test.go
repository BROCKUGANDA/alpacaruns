package risk

import (
	"context"
	"errors"
	"strings"
	"testing"
)
type fakeScorer struct {
	result FactorResult
	err    error
	calls  int
}

func (f *fakeScorer) ScoreFactors(ctx context.Context, symbol string) (FactorResult, error) {
	f.calls++
	return f.result, f.err
}

func passingFactors() FactorResult {
	return FactorResult{
		Composite: 0.85,
		MinScore:  0.6,
		Passed:    true,
		Factors:   map[string]float64{"trend": 0.9, "momentum": 0.8, "volatility": 0.9, "volume": 0.7, "sentiment": 0.5},
		Rationales: map[string]string{
			"trend": "above both MAs", "momentum": "+8%", "volatility": "calm",
			"volume": "1.2x avg", "sentiment": "neutral",
		},
	}
}

func failingFactors() FactorResult {
	return FactorResult{
		Composite: 0.41,
		MinScore:  0.6,
		Passed:    false,
		Factors:   map[string]float64{"trend": 0.2, "momentum": 0.3, "volatility": 0.4, "volume": 0.6, "sentiment": 0.55},
		Rationales: map[string]string{
			"trend": "below SMA50", "momentum": "-6%", "volatility": "2x threshold vol",
			"volume": "0.9x avg", "sentiment": "neutral",
		},
	}
}

func withFactorCfg(v *Validator) *Validator {
	v.Cfg.FactorMinScore = 0.6
	return v
}

func TestFactorGateAbsentScorerKeepsOldBehavior(t *testing.T) {
	// nil scorer: high confidence passes exactly as before, and the
	// verdict carries no factor attachment.
	v := newValidator(baseCfg(), fakeKill{false}, fakeClock{open: true, sessionKnown: true}, 100000, nil)
	got := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "5000", Confidence: conf(0.9)})
	if !got.Approved {
		t.Fatalf("expected approval without scorer, got %v", got.Reasons)
	}
	if got.Factors != nil {
		t.Fatalf("Factors must be nil when no scorer configured")
	}
}

func TestFactorGateIntegrationTable(t *testing.T) {
	tests := []struct {
		name       string
		scorer     *fakeScorer
		confidence float64
		approved   bool
		wantSubs   []string // substrings expected in rejection reasons
	}{
		{
			name:       "both gates pass",
			scorer:     &fakeScorer{result: passingFactors()},
			confidence: 0.9,
			approved:   true,
		},
		{
			name:       "good factors but low confidence rejected",
			scorer:     &fakeScorer{result: passingFactors()},
			confidence: 0.5,
			approved:   false,
			wantSubs:   []string{"confidence 0.50 < minimum 0.70"},
		},
		{
			name:       "high confidence but weak composite rejected with factor reasons",
			scorer:     &fakeScorer{result: failingFactors()},
			confidence: 0.95,
			approved:   false,
			wantSubs: []string{
				"composite factor score 0.41 < minimum 0.60",
				"weak factor trend=0.20",
				"weak factor volatility=0.40",
			},
		},
		{
			name:       "scorer error fails closed even at max confidence",
			scorer:     &fakeScorer{err: errors.New("bars endpoint down")},
			confidence: 0.99,
			approved:   false,
			wantSubs:   []string{"factor scoring unavailable"},
		},
		{
			name:       "boundary composite equal to minimum passes",
			scorer:     &fakeScorer{result: FactorResult{Composite: 0.6, MinScore: 0.6, Passed: true, Factors: map[string]float64{"trend": 0.6}}},
			confidence: 0.7,
			approved:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := withFactorCfg(newValidator(baseCfg(), fakeKill{false}, fakeClock{open: true, sessionKnown: true}, 100000, nil))
			v.Factors = tt.scorer
			p := Proposal{Symbol: "AAPL", Side: "buy", Notional: "1000"}
			if tt.confidence > 0 {
				p.Confidence = conf(tt.confidence)
			}
			got := v.Validate(p)
			if got.Approved != tt.approved {
				t.Fatalf("approved = %v (%v), want %v", got.Approved, got.Reasons, tt.approved)
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(got.String(), sub) {
					t.Fatalf("verdict %q missing %q", got.String(), sub)
				}
			}
			if tt.scorer.calls != 1 {
				t.Fatalf("scorer called %d times, want 1", tt.scorer.calls)
			}
		})
	}
}

func TestFactorGateAttachesResultOnVerdict(t *testing.T) {
	sc := &fakeScorer{result: passingFactors()}
	v := withFactorCfg(newValidator(baseCfg(), fakeKill{false}, fakeClock{open: true, sessionKnown: true}, 100000, nil))
	v.Factors = sc
	got := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "1000", Confidence: conf(0.9)})
	if got.Factors == nil {
		t.Fatal("verdict must carry factor result when scorer configured")
	}
	if got.Factors.Composite != 0.85 || !got.Factors.Passed {
		t.Fatalf("unexpected attached factors: %+v", got.Factors)
	}
	if len(got.Factors.Rationales["trend"]) == 0 {
		t.Fatal("rationales must be carried through to the verdict")
	}
}

func TestFactorGateMissingConfidenceStillRejectedWithScorer(t *testing.T) {
	// The multi-factor gate ADDS a requirement; it never relaxes the
	// existing confidence gate.
	sc := &fakeScorer{result: passingFactors()}
	v := withFactorCfg(newValidator(baseCfg(), fakeKill{false}, fakeClock{open: true, sessionKnown: true}, 100000, nil))
	v.Factors = sc
	got := v.Validate(Proposal{Symbol: "AAPL", Side: "buy", Notional: "1000"})
	if got.Approved {
		t.Fatal("missing confidence must still reject when scorer present")
	}
	if !strings.Contains(got.String(), "missing confidence score") {
		t.Fatalf("verdict missing confidence reason: %q", got.String())
	}
}
