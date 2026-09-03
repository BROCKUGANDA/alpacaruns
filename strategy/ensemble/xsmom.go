package ensemble

import (
	"context"
	"fmt"
	"sort"
)

// xsmom.go: cross-sectional momentum. Rank the WHOLE universe by its
// 20-session return each tick:
//
//	top TopN symbols      -> Buy (confidence by rank margin)
//	bottom-third + HELD   -> Sell
//	middle                -> Hold
//
// This is the only expert that needs the full dataset on every call,
// which is why Evaluate receives the whole MarketData and ignores the
// per-symbol argument.

const (
	xsMomWindow    = 20 // return lookback in sessions
	xsMomTopN      = 3
	xsMomBottomFrc = 3.0 // bottom 1/N of the universe counts as "bottom third"
	xsMomMinBars   = xsMomWindow + 1
)

// XSMomExpert is the relative-strength voice.
type XSMomExpert struct {
	TopN       int     // how many leaders to buy (default 3)
	MinHistory int     // minimum closes per symbol to be rankable
	BottomCut  float64 // fraction of universe treated as laggards (default 1/3)
}

// Name implements Expert.
func (*XSMomExpert) Name() string { return "xsmom" }

func newXSMom() *XSMomExpert {
	return &XSMomExpert{TopN: xsMomTopN, MinHistory: xsMomMinBars, BottomCut: 1 / xsMomBottomFrc}
}

// NewXSMomExpert validates defaults.
func NewXSMomExpert() *XSMomExpert { return newXSMom() }

// ranked is one symbol's momentum measurement.
type ranked struct {
	symbol string
	ret    float64 // N-session fractional return
}

// Evaluate ranks every symbol in data.Symbols by trailing return.
func (e *XSMomExpert) Evaluate(_ context.Context, _ string, data MarketData) ([]Signal, error) {
	if e.TopN <= 0 {
		e = newXSMom()
	}
	var rs []ranked
	for sym, sd := range data.Symbols {
		if sd == nil || len(sd.Closes) < e.MinHistory {
			continue
		}
		c := sd.Closes
		old := c[len(c)-1-e.MinHistory+1]
		if old == 0 {
			continue
		}
		rs = append(rs, ranked{symbol: sym, ret: c[len(c)-1]/old - 1})
	}
	if len(rs) == 0 {
		return []Signal{holdSig("universe", "xsmom: nothing rankable")}, nil
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].ret > rs[j].ret })

	var out []Signal
	n := len(rs)
	for i, r := range rs {
		switch {
		case i < e.TopN:
			// Confidence by rank margin: leader of a big spread scores high.
			margin := 0.5
			if n > 1 && r.ret != rs[n-1].ret {
				span := rs[0].ret - rs[n-1].ret
				if span > 0 {
					margin = Clamp01(0.5 + 0.5*(r.ret-rs[min(n-1, e.TopN)].ret)/span)
				}
			}
			out = append(out, Signal{Symbol: r.symbol, Action: ActionBuy,
				Confidence: margin, Regime: RegimeTrending,
				Reason: fmt.Sprintf("xsmom BUY rank %d/%d ret=%.2f%%", i+1, n, r.ret*100)})
		case float64(i) >= float64(n)*(1-e.BottomCut):
			if data.Held[r.symbol] {
				out = append(out, Signal{Symbol: r.symbol, Action: ActionSell,
					Confidence: 0.6, Regime: RegimeTrending,
					Reason: fmt.Sprintf("xsmom SELL held bottom-third rank %d/%d ret=%.2f%%", i+1, n, r.ret*100)})
			} else {
				out = append(out, Signal{Symbol: r.symbol, Action: ActionHold, Confidence: 0.5,
					Regime: RegimeTrending,
					Reason: fmt.Sprintf("xsmom hold bottom-third rank %d/%d", i+1, n)})
			}
		default:
			out = append(out, Signal{Symbol: r.symbol, Action: ActionHold, Confidence: 0.5,
				Regime: RegimeTrending,
				Reason: fmt.Sprintf("xsmom hold middle rank %d/%d ret=%.2f%%", i+1, n, r.ret*100)})
		}
	}
	return out, nil
}


// WholeUniverse marks this expert cross-sectional: invoked once per tick.
func (*XSMomExpert) WholeUniverse() bool { return true }

var _ Expert = (*XSMomExpert)(nil)
