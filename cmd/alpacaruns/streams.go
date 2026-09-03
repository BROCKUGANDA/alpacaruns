// Live-stream integration for monitor mode: market-data events are
// logged (ingest), and trade_updates fills are journaled into the pnl
// Journal the moment they arrive, so P/L is live without waiting for
// `pl`'s backfill. Both streams shut down cleanly on ctx cancel.
package main

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/stream"
)

// tradeStreamHost derives the trade_updates websocket host from the REST
// base URL: paper keys use the paper host, live keys the live one.
func tradeStreamHost(baseURL string) string {
	const live = "wss://api.alpaca.markets"
	if baseURL == "" {
		return "wss://paper-api.alpaca.markets"
	}
	for _, marker := range []string{"live", "broker"} {
		if containsFold(baseURL, marker) {
			return live
		}
	}
	return "wss://paper-api.alpaca.markets"
}

func containsFold(s, sub string) bool {
	n := len(s)
	m := len(sub)
	for i := 0; i+m <= n; i++ {
		if eqFold(s[i:i+m], sub) {
			return true
		}
	}
	return false
}

func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// startStreams spawns both streams in goroutines. Construction failures
// and permanent stream deaths are logged but never kill the monitor loop:
// each consumer runs in its own recovered goroutine.
func startStreams(ctx context.Context, cfg *config.Config) {
	logger := slog.Default()

	ms, err := stream.NewMarketStream(stream.MarketConfig{
		KeyID:   cfg.AlpacaKeyID,
		Secret:  cfg.AlpacaSecret,
		Feed:    cfg.StreamFeed,
		Symbols: defaultWatchlist(),
	}, logger)
	if err != nil {
		slog.Error("market stream unavailable; continuing without live data ingest", "err", err)
	} else {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("market stream panicked; stream stopped", "panic", r)
				}
			}()
			go consumeMarketEvents(ctx, ms.Events(), logger)
			if err := ms.Run(ctx); err != nil {
				slog.Error("market stream died permanently; continuing without live data ingest", "err", err)
			}
			ms.Close()
		}()
	}

	ts, err := stream.NewTradeStream(stream.TradeConfig{
		KeyID:  cfg.AlpacaKeyID,
		Secret: cfg.AlpacaSecret,
		Host:   tradeStreamHost(cfg.AlpacaBaseURL),
	}, logger)
	if err != nil {
		slog.Error("trade stream unavailable; fills will be backfilled by pl instead", "err", err)
	} else {
		j, jerr := openJournal(cfg)
		if jerr != nil {
			slog.Error("trade journal unavailable; fill streaming disabled", "err", jerr)
		} else {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("trade stream panicked; stream stopped", "panic", r)
					}
				}()
				go consumeFills(ctx, ts.Fills(), j, logger)
				if err := ts.Run(ctx); err != nil {
					slog.Error("trade stream died permanently; fills will be backfilled by pl instead", "err", err)
				}
				ts.Close()
				j.Close()
			}()
		}
	}
}

// defaultWatchlist returns the standard symbol set for the market feed.
func defaultWatchlist() []string {
	return []string{"AAPL", "MSFT", "NVDA"}
}

// consumeMarketEvents ingests trades/quotes/bars: every event is logged
// with symbol + price so live-data flow is observable. Runs until ctx is
// cancelled or the channel closes.
func consumeMarketEvents(ctx context.Context, events <-chan stream.MarketEvent, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch {
			case ev.Trade != nil:
				log.Info("market trade", "symbol", ev.Trade.Symbol, "price", ev.Trade.Price, "size", ev.Trade.Size)
			case ev.Quote != nil:
				log.Debug("market quote", "symbol", ev.Quote.Symbol,
					"bid", ev.Quote.BidPx, "ask", ev.Quote.AskPx)
			case ev.Bar != nil:
				log.Info("market bar", "symbol", ev.Bar.Symbol,
					"open", ev.Bar.Open, "close", ev.Bar.Close, "volume", ev.Bar.Volume)
			}
		}
	}
}

// consumeFills journals streamed fills immediately, deduplicating by
// order ID exactly like the pl backfill path so a fill already recorded
// is skipped, not double-counted.
func consumeFills(ctx context.Context, fills <-chan stream.FillEvent, j *pnl.Journal, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-fills:
			if !ok {
				return
			}
			if f.OrderID != "" && j.KnownOrder(f.OrderID) {
				continue // already journaled via backfill or earlier delivery
			}
			rec := pnl.Record{
				Kind:    pnl.KindFill,
				OrderID: f.OrderID,
				Symbol:  f.Symbol,
				Side:    f.Side,
				Qty:     strconv.FormatFloat(f.Qty, 'f', -1, 64),
				Price:   strconv.FormatFloat(f.Price, 'f', -1, 64),
				Status:  "filled",
			}
			if f.TS != 0 {
				rec.TS = time.Unix(0, f.TS).UTC()
			}
			if err := j.Append(rec); err != nil {
				log.Error("fill journal append failed", "order_id", f.OrderID, "err", err)
				continue
			}
			log.Info("fill journaled", "symbol", f.Symbol, "side", f.Side,
				"qty", rec.Qty, "price", rec.Price, "order_id", f.OrderID)
		}
	}
}
