package picker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// timeNowUTC is the clock used by the picker; overridable in tests.
var timeNowUTC = func() time.Time { return time.Now().UTC() }

// ClassBars fetches bars for a list of instruments, routing each to
// the endpoint family its class requires: equities, ETFs and index
// trackers come from the stock bars endpoint, crypto pairs from the
// crypto endpoint. Options have no bar series of their own here —
// their signal is the underlying's.
type ClassBars struct {
	C *tools.Client
}

// GetBars implements factors.BarSource for a mixed-class universe.
func (cb ClassBars) GetBars(ctx context.Context, symbols []string, timeframe, start, end string, limit int) (map[string][]tools.Bar, error) {
	var stockSyms, cryptoSyms []string
	for _, s := range symbols {
		if isCryptoForm(s) {
			cryptoSyms = append(cryptoSyms, s)
		} else {
			stockSyms = append(stockSyms, s)
		}
	}
	out := map[string][]tools.Bar{}
	if len(stockSyms) > 0 {
		res, err := cb.C.GetBars(ctx, stockSyms, timeframe, start, end, limit)
		if err != nil {
			return nil, fmt.Errorf("stock bars: %w", err)
		}
		for k, v := range res {
			out[k] = v
		}
	}
	if len(cryptoSyms) > 0 {
		res, err := cb.C.GetCryptoBars(ctx, cryptoSyms, timeframe, start, end, limit)
		if err != nil {
			return nil, fmt.Errorf("crypto bars: %w", err)
		}
		for k, v := range res {
			out[k] = v
		}
	}
	return out, nil
}

// isCryptoForm reports whether a symbol is a crypto pair.
func isCryptoForm(s string) bool {
	return strings.Contains(strings.ToUpper(s), "/")
}

// Scanner turns a registry into ranked candidates. It owns the data
// ingestion for every asset class: bars, live marks, and (for options)
// the contract greeks.
type Scanner struct {
	// Client is the broker client. Required.
	Client *tools.Client
	// Scorer scores an equity-like symbol (equity, ETF, index tracker
	// or an option's underlying). Optional: without it only live marks
	// are reported.
	Scorer factors.Scorer
	// MinComposite is the composite a candidate must clear to be
	// marked "buy". Defaults to the scorer's own floor when the
	// scorer is set.
	MinComposite float64
	// Timeframe is the bar timeframe used for scoring.
	Timeframe string
}

// Candidate is one scanned instrument with its signal.
type Candidate struct {
	Instrument Instrument         `json:"instrument"`
	Composite  float64            `json:"composite"`
	Factors    map[string]float64 `json:"factors,omitempty"`
	Rationales map[string]string  `json:"rationales,omitempty"`
	Price      float64            `json:"price"`
	Signal     string             `json:"signal"` // buy | hold | sell
	Reason     string             `json:"reason"`
	// Error carries the reason a candidate could not be scored. A
	// candidate with an error is reported but never actionable.
	Error string `json:"error,omitempty"`
}

// Scan scores every instrument in the registry and returns the
// candidates ranked by composite score (best first).
func (s *Scanner) Scan(ctx context.Context, reg *Registry) ([]Candidate, error) {
	if reg == nil {
		return nil, nil
	}
	if s.Client == nil {
		return nil, fmt.Errorf("picker: no broker client")
	}
	tf := s.Timeframe
	if tf == "" {
		tf = factors.DefaultTimeframe
	}

	// Options are scored on their underlying: an option has no series
	// of its own, and the contract selection already encodes the
	// delta/expiry preference.
	scoreTargets := map[string]Instrument{}
	for _, inst := range reg.Instruments() {
		switch inst.Class {
		case ClassOption:
			if inst.Underlying != "" {
				scoreTargets[inst.Underlying] = inst
			}
		default:
			scoreTargets[inst.Symbol] = inst
		}
	}

	targets := make([]string, 0, len(scoreTargets))
	for sym := range scoreTargets {
		targets = append(targets, sym)
	}
	sort.Strings(targets)

	// Live marks for everything priced as a share/coin.
	prices, err := s.marks(ctx, scoreTargets)
	if err != nil {
		return nil, err
	}

	out := make([]Candidate, 0, len(scoreTargets))
	for _, sym := range targets {
		inst := scoreTargets[sym]
		cand := Candidate{Instrument: inst, Signal: "hold", Price: prices[sym]}
		if s.Scorer != nil {
			res, err := s.Scorer.Score(ctx, sym)
			if err != nil {
				cand.Error = err.Error()
				cand.Reason = "scoring failed"
			} else {
				cand.Composite = res.Composite
				cand.Factors = map[string]float64{}
				cand.Rationales = map[string]string{}
				for name, fr := range res.Factors {
					cand.Factors[name] = fr.Score
					cand.Rationales[name] = fr.Rationale
				}
				min := s.MinComposite
				if min <= 0 {
					min = 0.6
				}
				switch {
				case res.Composite >= min:
					cand.Signal = "buy"
					cand.Reason = fmt.Sprintf("composite %.2f >= %.2f", res.Composite, min)
				case res.Composite < 0.4:
					cand.Signal = "sell"
					cand.Reason = fmt.Sprintf("composite %.2f below 0.40", res.Composite)
				default:
					cand.Signal = "hold"
					cand.Reason = fmt.Sprintf("composite %.2f below %.2f", res.Composite, min)
				}
			}
		} else {
			cand.Reason = "no scorer configured"
		}
		out = append(out, cand)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Composite > out[j].Composite })
	return out, nil
}

// marks fetches the live mark for each scored symbol, routing crypto
// pairs to the crypto snapshot endpoint.
func (s *Scanner) marks(ctx context.Context, targets map[string]Instrument) (map[string]float64, error) {
	var stockSyms, cryptoSyms []string
	for sym := range targets {
		if isCryptoForm(sym) {
			cryptoSyms = append(cryptoSyms, sym)
		} else {
			stockSyms = append(stockSyms, sym)
		}
	}
	out := map[string]float64{}
	if len(stockSyms) > 0 {
		snaps, err := s.Client.GetSnapshots(ctx, stockSyms)
		if err != nil {
			return nil, fmt.Errorf("snapshots: %w", err)
		}
		for sym, sn := range snaps {
			out[sym] = sn.MidPrice()
		}
	}
	if len(cryptoSyms) > 0 {
		snaps, err := s.Client.GetCryptoSnapshots(ctx, cryptoSyms)
		if err != nil {
			return nil, fmt.Errorf("crypto snapshots: %w", err)
		}
		for sym, sn := range snaps {
			px := sn.LatestTrade
			if px <= 0 {
				px = sn.DailyBarClose
			}
			out[sym] = px
		}
	}
	return out, nil
}
