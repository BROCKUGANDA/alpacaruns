package strategy

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/options"
)

// OptionSource is the slice of options.Client the overlay consumes.
type OptionSource interface {
	GetContracts(ctx context.Context, q options.ContractsQuery) ([]options.Contract, string, error)
	GetSnapshots(ctx context.Context, symbols []string) (map[string]options.Snapshot, error)
}

// OptionPlanner picks a deep-ITM options substitute for an equity entry:
// calls for BUY signals (puts would apply to short entries this engine
// never opens), expiring 30-45 days out, |delta| inside the configured
// band, contracts sized so total premium stays within the position
// budget. Any data gap returns nil + reason — the equity leg is the
// fallback, never an error.
type OptionPlanner struct {
	Opts     OptionSource
	DeltaMin float64 // |delta| lower bound (e.g. 0.60)
	DeltaMax float64 // |delta| upper bound (e.g. 0.70)
	MinDTE   int
	MaxDTE   int
	Sizing   Sizing
	Log      *log.Logger
	now      func() time.Time
}

// NewOptionPlanner validates wiring.
func NewOptionPlanner(o OptionSource, s Settings, sizing Sizing) (*OptionPlanner, error) {
	if o == nil {
		return nil, fmt.Errorf("options planner needs an options source")
	}
	if s.OptDeltaMin <= 0 || s.OptDeltaMin > s.OptDeltaMax || s.OptDeltaMax >= 1 {
		return nil, fmt.Errorf("delta band invalid: [%g,%g]", s.OptDeltaMin, s.OptDeltaMax)
	}
	lg := log.Default()
	return &OptionPlanner{
		Opts: o, DeltaMin: s.OptDeltaMin, DeltaMax: s.OptDeltaMax,
		MinDTE: s.OptMinDTE, MaxDTE: s.OptMaxDTE, Sizing: sizing, Log: lg,
		now: time.Now,
	}, nil
}

// Plan returns an option leg for an equity BUY on underlying at spot, or
// nil with a human-readable skip reason when nothing qualifies.
func (p *OptionPlanner) Plan(ctx context.Context, underlying string, portfolioValue float64) (*OptionLeg, string) {
	now := p.now().UTC()
	gte := now.AddDate(0, 0, p.MinDTE).Format("2006-01-02")
	lte := now.AddDate(0, 0, p.MaxDTE).Format("2006-01-02")

	contracts, _, err := p.Opts.GetContracts(ctx, options.ContractsQuery{
		UnderlyingSymbols: []string{underlying},
		Type:              "call",
		Status:            "active",
		ExpirationDateGTE: gte,
		ExpirationDateLTE: lte,
		Limit:             500,
	})
	if err != nil {
		return nil, fmt.Sprintf("chain fetch failed (%v)", err)
	}

	// Rank candidates by distance from the middle of the delta band:
	// deep-ITM ~60-70 delta means high strike-relative leverage with
	// less extrinsic decay than ATM.
	type cand struct {
		c     options.Contract
		delta float64
		mid   float64
		dist  float64
	}
	var cands []cand
	bandMid := (p.DeltaMin + p.DeltaMax) / 2
	for _, c := range contracts {
		if !c.Tradable || strings.ToUpper(c.Type) != "call" {
			continue
		}
		snaps, err := p.Opts.GetSnapshots(ctx, []string{c.Symbol})
		if err != nil {
			continue
		}
		snap, ok := snaps[c.Symbol]
		if !ok {
			continue
		}
		delta := math.Abs(snap.Greeks.Delta)
		if delta < p.DeltaMin || delta > p.DeltaMax {
			continue
		}
		premium := snap.MidQuote()
		if premium <= 0 {
			continue
		}
		d := math.Abs(delta - bandMid)
		cands = append(cands, cand{c: c, delta: delta, mid: premium, dist: d})
	}
	if len(cands) == 0 {
		return nil, fmt.Sprintf("no tradable call in DTE %d-%d with delta %.2f-%.2f", p.MinDTE, p.MaxDTE, p.DeltaMin, p.DeltaMax)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })

	best := cands[0]
	qr := p.Sizing.SizeOptionContract(portfolioValue, best.mid)
	if qr.Qty < 1 {
		return nil, qr.Skip
	}
	p.Log.Printf("[strategy] option overlay %s: %d x %s delta=%.2f premium=%.2f",
		underlying, qr.Qty, best.c.Symbol, best.delta, best.mid)
	return &OptionLeg{
		OCC: best.c.Symbol, Type: "call", Delta: best.delta,
		Premium: best.mid, Contracts: qr.Qty, Expiry: best.c.ExpirationDate,
	}, ""
}

// OptionLeg mirrors engine.go's plan type; defined once here and aliased.
type OptionLeg struct {
	OCC       string
	Type      string
	Delta     float64
	Premium   float64
	Contracts int
	Expiry    string
}
