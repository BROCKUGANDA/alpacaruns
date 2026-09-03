package strategy

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/agents"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// Executor turns EntryPlans into real (paper) orders through the SAME
// deterministic risk gate as every other path (agents.NewValidator), so
// all caps, kill-switch and session rules apply. Crypto entries never
// carry bracket children (unsupported by Alpaca on crypto); equities get
// order_class=bracket with TP/SL children.
type Executor struct {
	Client    *tools.Client
	Validator RiskGate
	Kill      *agents.KillSwitch
	State     *StateStore
	Journal   *pnl.Journal
	Log       *log.Logger
}

// RiskGate is the slice of *risk.Validator the executor consumes;
// *risk.Validator satisfies it directly and tests fake it.
type RiskGate interface {
	Validate(p risk.Proposal) risk.Verdict
}

func (x *Executor) ExecuteEntry(ctx context.Context, p EntryPlan, conf float64) error {
	isNotional := p.Notional > 0
	if isNotional && !p.Crypto {
		return fmt.Errorf("notional orders only supported for crypto (got %s)", p.Symbol)
	}
	// Crypto notional orders must be day orders (Alpaca rejects
	// fractional notional with GTC). For qty-based crypto orders, the
	// default GTC is fine.
	// Crypto notional orders: Alpaca paper accepts GTC, not day. Verified
	// empirically (day returns 422 "invalid crypto time_in_force").
	tif := "day"
	if isNotional {
		tif = "gtc"
	}
	req := tools.OrderRequest{
		Symbol:      p.Symbol,
		Side:        "buy",
		Type:        "market",
		TimeInForce: tif,
	}
	if isNotional {
		req.Notional = strconv.FormatFloat(p.Notional, 'f', 2, 64)
	} else {
		req.Qty = strconv.Itoa(p.Qty)
	}
	verdict := x.Validator.Validate(risk.Proposal{
		Symbol: p.Symbol, Side: "buy", Qty: req.Qty, Notional: req.Notional,
		Confidence: &conf, OrderType: req.Type, TimeInForce: req.TimeInForce,
	})
	if !verdict.Approved {
		return fmt.Errorf("risk gate rejected %s entry: %s", p.Symbol, strings.Join(verdict.Reasons, "; "))
	}
	o, err := x.placeWithBrackets(ctx, req, p)
	if err != nil {
		return fmt.Errorf("place %s entry: %w", p.Symbol, err)
	}
	x.log().Printf("[strategy] ENTRY %s qty=%s notional=%.2f @~%.4f budget=%.2f crypto=%v tp=%.4f sl=%.4f order=%s",
		p.Symbol, req.Qty, p.Notional, p.Price, p.Budget, p.Crypto, p.Brackets.TakeProfit, p.Brackets.StopLoss, o.ID)
	if x.Journal != nil {
		_ = x.Journal.Append(pnl.Record{
			Kind: pnl.KindDecision, Source: "strategy:auto", Risk: "APPROVED",
			Confidence: &conf,
			Detail: fmt.Sprintf("entry %s qty=%s notional=%.2f price=%.4f tp=%.4f sl=%.4f crypto=%t order=%s",
				p.Symbol, req.Qty, p.Notional, p.Price, p.Brackets.TakeProfit, p.Brackets.StopLoss, p.Crypto, o.ID),
		})
	}
	if p.Crypto {
		if err := x.State.SetLevel(PositionLevels{
			Symbol: p.Symbol, EntryPrice: p.Price,
			TakeProfit: p.Brackets.TakeProfit, StopLoss: p.Brackets.StopLoss,
			Qty: req.Qty, Since: time.Now().UTC(),
		}); err != nil {
			x.log().Printf("[strategy] WARNING: %s filled but state persist failed: %v", p.Symbol, err)
		}
	}
	return nil
}


// placeWithBrackets issues the order request. Equities get the bracket
// envelope merged into the JSON body via a typed extension of
// OrderRequest — Alpaca accepts unknown-order fields only through the
// raw JSON, so we marshal manually with the extra keys.
func (x *Executor) placeWithBrackets(ctx context.Context, req tools.OrderRequest, p EntryPlan) (*tools.Order, error) {
	if p.Crypto {
		return x.Client.PlaceOrder(ctx, req)
	}
	// order_class=bracket is EQUITIES ONLY (Alpaca rejects it on crypto
	// orders). Build the full JSON body including the bracket envelope;
	// tools.OrderRequest has no class field, so extend at the wire level.
	body := map[string]any{
		"symbol":        req.Symbol,
		"qty":           req.Qty,
		"side":          req.Side,
		"type":          req.Type,
		"time_in_force": req.TimeInForce,
		"order_class":   "bracket",
		"take_profit":   map[string]string{"limit_price": fmt.Sprintf("%.4f", p.Brackets.TakeProfit)},
		"stop_loss":     map[string]string{"stop_price": fmt.Sprintf("%.4f", p.Brackets.StopLoss)},
	}
	if req.LimitPrice != "" {
		body["limit_price"] = req.LimitPrice
	}
	if req.ClientOrderID != "" {
		body["client_order_id"] = req.ClientOrderID
	}
	var out tools.Order
	endpoint := x.Client.BaseURL + "/orders"
	if err := x.Client.Do(ctx, "POST", endpoint, nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExecutePreOrder queues an equity entry as a resting limit/GTC bracket
// while the market is closed (PRE_ORDERS mode). The entry limit is the
// last daily close; TP/SL children ride the same bracket envelope and
// only activate once the parent fills at/after the next open. The risk
// validator's session gate must have PRE_ORDERS enabled or this rejects.
func (x *Executor) ExecutePreOrder(ctx context.Context, p EntryPlan, conf float64) error {
	req := tools.OrderRequest{
		Symbol:      p.Symbol,
		Side:        "buy",
		Type:        "limit",
		TimeInForce: "gtc",
		Qty:         strconv.Itoa(p.Qty),
		LimitPrice:  fmt.Sprintf("%.4f", p.Price),
	}
	verdict := x.Validator.Validate(risk.Proposal{
		Symbol: p.Symbol, Side: "buy", Qty: req.Qty,
		Confidence: &conf, OrderType: req.Type, TimeInForce: req.TimeInForce,
	})
	if !verdict.Approved {
		return fmt.Errorf("risk gate rejected %s pre-order: %s", p.Symbol, strings.Join(verdict.Reasons, "; "))
	}
	o, err := x.placeWithBrackets(ctx, req, p)
	if err != nil {
		return fmt.Errorf("place %s pre-order: %w", p.Symbol, err)
	}
	x.log().Printf("[strategy] PRE-ORDER %s qty=%d @limit %.4f budget=%.2f tp=%.4f sl=%.4f order=%s (resting until open)",
		p.Symbol, p.Qty, p.Price, p.Budget, p.Brackets.TakeProfit, p.Brackets.StopLoss, o.ID)
	if x.Journal != nil {
		_ = x.Journal.Append(pnl.Record{
			Kind: pnl.KindDecision, Source: "strategy:auto", Risk: "APPROVED",
			Confidence: &conf,
			Detail: fmt.Sprintf("pre-order %s qty=%d limit=%.4f tp=%.4f sl=%.4f order=%s",
				p.Symbol, p.Qty, p.Price, p.Brackets.TakeProfit, p.Brackets.StopLoss, o.ID),
		})
	}
	return nil
}

func (x *Executor) log() *log.Logger {
	if x.Log == nil {
		return log.Default()
	}
	return x.Log
}
