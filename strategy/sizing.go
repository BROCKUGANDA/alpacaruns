package strategy

import (
	"fmt"
	"math"
)

// Sizing computes the fixed-fractional position budget and share/contract
// quantity for one entry. Pure math, fully table-testable.
//
//	budget = portfolioValue * PositionPct, capped at MaxPositionUSD
//	qty    = floor(budget / price); skip when qty < 1
type Sizing struct {
	PositionPct    float64
	MaxPositionUSD float64
}

// Budget returns the USD budget for a new position.
func (s Sizing) Budget(portfolioValue float64) float64 {
	return min(s.PositionPct*portfolioValue, s.MaxPositionUSD)
}

// QtyResult is the outcome of sizing one entry.
type QtyResult struct {
	Qty    int     // shares or whole contracts; 0 = skip (or notional order)
	Budget float64 // USD allocated to this entry
	Skip   string  // non-empty human-readable reason when Qty == 0
}

// SizeForPrice sizes an equity or crypto entry at the given mark.
func (s Sizing) SizeForPrice(portfolioValue, price float64) QtyResult {
	if price <= 0 {
		return QtyResult{Skip: fmt.Sprintf("non-positive price %g", price)}
	}
	if portfolioValue <= 0 {
		return QtyResult{Skip: fmt.Sprintf("non-positive portfolio value %g", portfolioValue)}
	}
	budget := s.Budget(portfolioValue)
	// The epsilon is not cosmetic. budget/price is float64 arithmetic, and
	// a budget that lands exactly on an integer number of units routinely
	// computes as 28.999999999999996 — truncation then quietly buys 28 when
	// the operator sized 29. A tolerance far below one unit only ever
	// rescues a value that was meant to be whole.
	qty := int(math.Floor(budget/price + 1e-9))
	if qty < 1 {
		return QtyResult{
			Budget: budget,
			Skip: fmt.Sprintf("budget %.2f below one unit at price %.2f (POSITION_PCT=%g, cap %.2f)",
				budget, price, s.PositionPct, s.MaxPositionUSD),
		}
	}
	return QtyResult{Qty: qty, Budget: budget}
}

// SizeNotional is used for crypto entries placed as USD-notional orders
// (fractional BTC/ETH). The whole budget is allocated as a notional
// amount; Alpaca fills whatever fraction of a coin that buys. Returns
// Skip when budget or price are non-positive.
func (s Sizing) SizeNotional(portfolioValue, price float64) QtyResult {
	if price <= 0 {
		return QtyResult{Skip: fmt.Sprintf("non-positive price %g", price)}
	}
	if portfolioValue <= 0 {
		return QtyResult{Skip: fmt.Sprintf("non-positive portfolio value %g", portfolioValue)}
	}
	budget := s.Budget(portfolioValue)
	if budget <= 0 {
		return QtyResult{Skip: "zero budget"}
	}
	return QtyResult{Qty: 0, Budget: budget} // caller uses .Budget as notional
}

// SizeOptionContract sizes ONE options contract entry: contracts sized so
// the premium outlay stays within the position budget. Returns Skip when
// even a single contract exceeds the budget — never fractional contracts.
func (s Sizing) SizeOptionContract(portfolioValue, contractPremium float64) QtyResult {
	if contractPremium <= 0 {
		return QtyResult{Skip: fmt.Sprintf("non-positive premium %g", contractPremium)}
	}
	if portfolioValue <= 0 {
		return QtyResult{Skip: fmt.Sprintf("non-positive portfolio value %g", portfolioValue)}
	}
	budget := s.Budget(portfolioValue)
	if contractPremium > budget {
		return QtyResult{
			Budget: budget,
			Skip: fmt.Sprintf("one contract costs %.2f, above budget %.2f (cap %.2f)",
				contractPremium, budget, s.MaxPositionUSD),
		}
	}
	n := int(budget / contractPremium)
	if n < 1 {
		return QtyResult{Budget: budget, Skip: "fewer than one contract affordable"}
	}
	return QtyResult{Qty: n, Budget: budget}
}
