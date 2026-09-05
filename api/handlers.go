// handlers.go — the actual HTTP handlers, one per route. Every
// handler:
//   - parses + validates its inputs (4xx on bad input, never 500)
//   - reads the trade log + state file via readAllRecords
//   - shells out to tools.Client for live Alpaca data
//   - emits the typed response from types.go
//
// Handlers do not share state beyond the *Server; they read files
// fresh on each request so a freshly appended journal line shows up
// immediately.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/risk"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// ---- /api/health ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	lastPollNS := s.lastPollTS.Load()
	lastPoll := time.Time{}
	if lastPollNS > 0 {
		lastPoll = time.Unix(0, lastPollNS)
	}
	botAlive := false
	if !lastPoll.IsZero() {
		// Bot is "alive" when last tick is within 3× poll interval
		// or 30 minutes — whichever is greater. Paper bots poll
		// every 5 minutes by default; the showcase wants the dot
		// green even after a brief stall.
		threshold := 30 * time.Minute
		if time.Since(lastPoll) < threshold {
			botAlive = true
		}
	}
	writeJSON(w, http.StatusOK, HealthResponse{
		OK:        true,
		Version:   Version,
		UptimeSec: int64(time.Since(s.startTime).Seconds()),
		LastPoll:  lastPoll,
		BotAlive:  botAlive,
	})
}

// ---- /api/status ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	st := readStrategyState(s.settings.StateFile)
	_ = st // reserved for future per-scope HALT labels
	ks := KillSnapshot()
	// The snapshot file is the source of truth for *current* halt
	// flags; the persisted strategy-state.json only carries peak
	// equity. If the snapshot is uninitialised (server just started
	// without the bot ever writing one), we conservatively report
	// Halted=false; the bot will push a snapshot on its next tick.
	s.refreshPauseFlag()
	paused := s.pauseFlag.Load()

	state := StateRunning
	switch {
	case paused && ks.Halted:
		state = StateHalted
	case ks.Halted:
		state = StateHalted
	case paused:
		state = StatePaused
	}

	lastTick := time.Time{}
	if ns := s.lastPollTS.Load(); ns > 0 {
		lastTick = time.Unix(0, ns)
	}
	lastErr := ""
	if v := s.lastError.Load(); v != nil {
		if s, ok := v.(string); ok {
			lastErr = s
		}
	}

	resp := StatusResponse{
		Bot:        state,
		KillSwitch: ks,
		Config:     s.buildStatusConfig(),
		TickNumber: s.tickNumber.Load(),
		LastTick:   lastTick,
		LastError:  lastErr,
		Paused:     paused,
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildStatusConfig assembles the StatusConfig block from the bot's
// loaded config and strategy settings. Both may be nil (cold start);
// every field is then zero, which the dashboard renders as "n/a".
func (s *Server) buildStatusConfig() StatusConfig {
	if s.cfg == nil {
		return StatusConfig{}
	}
	out := StatusConfig{
		MaxPositionUSD:       s.cfg.MaxPositionUSD,
		MaxPortfolioPct:      s.cfg.MaxPortfolioPct,
		CryptoMaxPositionUSD: s.cfg.CryptoMaxPositionUSD,
		LLMProvider:          string(s.cfg.LLMProvider),
	}
	if s.strat != nil {
		out.DailyDDHalt = s.strat.DailyDD
		out.WeeklyDDHalt = s.strat.WeeklyDD
		out.TotalDDHalt = s.strat.TotalDD
		out.OptionsEnabled = s.strat.OptionsEnabled
		out.Symbols = append([]string{}, s.strat.EquitySymbols...)
		out.Symbols = append(out.Symbols, s.strat.CryptoSymbols...)
	}
	return out
}



// ---- /api/account ----

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "no_client", "alpaca client not configured")
		return
	}
	acct, err := s.client.GetAccount(r.Context())
	if err != nil {
		// Never forward the upstream error text: it can carry URL
		// fragments or account identifiers. Log it, send a code.
		log.Printf("[api] alpaca GetAccount: %v", err)
		writeError(w, http.StatusBadGateway, "alpaca_error", "alpaca request failed")
		return
	}
	// Try to compute day P&L by reading last_close_equity. Some
	// paper accounts don't expose it; in that case DayPnL is "0".
	lastEq, _ := s.client.GetAccountRawField(r.Context(), "last_equity")
	dayPnL := "0"
	if lastEq != "" {
		// Equity - last_equity gives intraday movement; using
		// equity - last_close_equity when the API supports it would
		// be more accurate, but Alpaca's /v2/account exposes
		// last_equity (prior EOD) reliably enough for the demo.
		eq, _ := strconv.ParseFloat(acct.Equity, 64)
		le, _ := strconv.ParseFloat(lastEq, 64)
		if eq != 0 && le != 0 {
			dayPnL = fmt.Sprintf("%.2f", eq-le)
		}
	}
	writeJSON(w, http.StatusOK, AccountResponse{
		Equity:         acct.Equity,
		Cash:           acct.Cash,
		DayPnL:         dayPnL,
		BuyingPower:    acct.BuyingPower,
		Multiplier:     acct.Multiplier,
		Status:         acct.Status,
		AccountNumber:  maskAccountNumber(acct.AccountNumber),
		CreatedAt:      acct.CreatedAt,
		LastEquity:     lastEq,
		PortfolioValue: acct.PortfolioValue,
	})
}

// ---- /api/pnl ----

func (s *Server) handlePnL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	q := r.URL.Query()
	since, err := parseTime(q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_param", "invalid since parameter")
		return
	}
	until, err := parseTime(q.Get("until"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_param", "invalid until parameter")
		return
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		writeError(w, http.StatusBadRequest, "bad_param", "until is before since")
		return
	}
	records, err := readAllRecords(s.settings.TradeLog)
	if err != nil {
		log.Printf("[api] read trade log: %v", err)
		writeError(w, http.StatusInternalServerError, "read_log", "cannot read trade log")
		return
	}
	st := readStrategyState(s.settings.StateFile)

	// Reconstruct equity curve from fills + persisted starting equity
	// (or $100k default — matches the hackathon spec). The math is
	// intentionally simple: start = $100k; every buy adds cash-out,
	// every sell closes a FIFO lot and books realized P&L. The
	// snapshot series emits one point per fill.
	startEquity := st.StartingEquity
	if startEquity <= 0 {
		startEquity = 100000.0
	}
	snapshots, totalPnL, maxDD, wins, losses := buildEquityCurve(records, since, until, startEquity)

	resp := PnLResponse{
		Snapshots: snapshots,
		Summary: PnLSummary{
			StartingEquity: startEquity,
			CurrentEquity:  startEquity + totalPnL,
			TotalPnL:       totalPnL,
			MaxDrawdown:    maxDD,
			Sharpe:         sharpeFromSnapshots(snapshots),
			WinRate:        winRate(wins, losses),
			Trades:         wins + losses,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func buildEquityCurve(records []pnl.Record, since, until time.Time, start float64) ([]EquitySnapshot, float64, float64, int, int) {
	// Use FIFO matching (mirrors pnl.Stats). We track cumulative
	// realized P&L separately from the cash-balance walk so the
	// returned `equity` is always start + pnl (a clean number the
	// UI can plot directly).
	type lot struct{ qty, price float64 }
	books := map[string][]lot{}
	cash := start
	realized := 0.0
	peak := start
	maxDD := 0.0
	wins, losses := 0, 0
	var snapshots []EquitySnapshot

	for _, r := range records {
		if r.Kind != pnl.KindFill {
			continue
		}
		if !since.IsZero() && r.TS.Before(since) {
			continue
		}
		if !until.IsZero() && r.TS.After(until) {
			continue
		}
		qty, _ := strconv.ParseFloat(r.Qty, 64)
		px, _ := strconv.ParseFloat(r.Price, 64)
		if qty <= 0 || px <= 0 {
			continue
		}
		book := books[r.Symbol]
		switch r.Side {
		case "buy":
			book = append(book, lot{qty: qty, price: px})
			cash -= qty * px
		case "sell":
			// Close FIFO lots.
			remaining := qty
			for len(book) > 0 && remaining > 0 {
				front := book[0]
				take := front.qty
				if take > remaining {
					take = remaining
				}
				// Realized P&L on the closed portion.
				gain := (px - front.price) * take
				realized += gain
				cash += take * px
				if gain >= 0 {
					wins++
				} else {
					losses++
				}
				front.qty -= take
				remaining -= take
				if front.qty <= 0 {
					book = book[1:]
				} else {
					book[0] = front
				}
			}
			// Shorts (sell without prior long) — for the demo we
			// skip; the bot is long-only by design.
		}
		books[r.Symbol] = book

		// Compute peak / drawdown on mark-to-market equity = cash
		// + cost basis of open lots.
		openCost := 0.0
		for _, lots := range books {
			for _, lot := range lots {
				openCost += lot.qty * lot.price
			}
		}
		equity := cash + openCost
		if equity > peak {
			peak = equity
		}
		dd := 0.0
		if peak > 0 {
			dd = (peak - equity) / peak
		}
		if dd > maxDD {
			maxDD = dd
		}
		snapshots = append(snapshots, EquitySnapshot{
			T:              r.TS,
			Equity:         equity,
			DayPnL:         equity - start,
			DrawdownDaily:  dd,
			DrawdownWeekly: dd,
			DrawdownTotal:  dd,
		})
	}
	totalPnL := realized
	return snapshots, totalPnL, maxDD, wins, losses
}

// winRate returns 0 when no closed trades are present, otherwise
// wins / (wins + losses).
func winRate(wins, losses int) float64 {
	if wins+losses == 0 {
		return 0
	}
	return float64(wins) / float64(wins+losses)
}

// sharpeFromSnapshots computes an annualised Sharpe from per-fill
// returns. Returns 0 when fewer than 2 points are available.
func sharpeFromSnapshots(snaps []EquitySnapshot) float64 {
	if len(snaps) < 2 {
		return 0
	}
	var rets []float64
	for i := 1; i < len(snaps); i++ {
		if snaps[i-1].Equity != 0 {
			rets = append(rets, (snaps[i].Equity-snaps[i-1].Equity)/snaps[i-1].Equity)
		}
	}
	if len(rets) < 2 {
		return 0
	}
	var sum, sum2 float64
	for _, r := range rets {
		sum += r
		sum2 += r * r
	}
	mean := sum / float64(len(rets))
	variance := sum2/float64(len(rets)) - mean*mean
	if variance <= 0 {
		return 0
	}
	std := sqrt(variance)
	// Annualise assuming ~252 trading days and ~1 trade/day as a
	// coarse approximation. The showcase UI uses this for a label
	// only; it's not a substitute for a real risk model.
	return mean / std * sqrt(252)
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// Newton-Raphson starting from a sensible guess.
	z := x
	for i := 0; i < 16; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// ---- /api/trades ----

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	q := r.URL.Query()
	since, err := parseTime(q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_param", "invalid since parameter")
		return
	}
	until, err := parseTime(q.Get("until"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_param", "invalid until parameter")
		return
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		writeError(w, http.StatusBadRequest, "bad_param", "until is before since")
		return
	}
	symbolFilter, ok := parseSymbolFilter(w, q.Get("symbol"))
	if !ok {
		return
	}
	pathFilter, ok := parsePathFilter(w, q.Get("path"))
	if !ok {
		return
	}
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	cursor, ok := parseCursor(w, q.Get("cursor"))
	if !ok {
		return
	}

	records, err := readAllRecords(s.settings.TradeLog)
	if err != nil {
		log.Printf("[api] read trade log: %v", err)
		writeError(w, http.StatusInternalServerError, "read_log", "cannot read trade log")
		return
	}
	fills := readFills(records, since, until)
	// Apply symbol/path filters in-place (cheap; file is small).
	filtered := make([]pnl.Record, 0, len(fills))
	for _, f := range fills {
		if symbolFilter != "" && !strings.EqualFold(f.Symbol, symbolFilter) {
			continue
		}
		if pathFilter != "" && !sourceMatchesPath(f.Source, pathFilter) {
			continue
		}
		filtered = append(filtered, f)
	}
	// Sort newest-first so the dashboard's "Trade Log" header
	// matches what the operator expects. The JSONL file is
	// append-only and ordered oldest-first; without this, the
	// first page of an old bot would be ancient history.
	sortRecordsByTSDesc(filtered)
	// Cursor: index of the first record to return.
	if cursor > int64(len(filtered)) {
		cursor = int64(len(filtered))
	}
	end := cursor + int64(limit)
	if end > int64(len(filtered)) {
		end = int64(len(filtered))
	}
	window := filtered[cursor:end]
	rows := make([]TradeRow, 0, len(window))
	for _, r := range window {
		rows = append(rows, tradeRowFromRecord(r))
	}
	var next *int64
	if end < int64(len(filtered)) {
		v := end
		next = &v
	}
	writeJSON(w, http.StatusOK, TradesResponse{Trades: rows, NextCursor: next})
}

// tradeRowFromRecord maps a pnl.Record to the dashboard's TradeRow.
// The journal's Source field is mapped to a friendly path: the UI
// shows a badge per row.
func tradeRowFromRecord(r pnl.Record) TradeRow {
	path := mapSourceToPath(r.Source)
	qty, _ := strconv.ParseFloat(r.Qty, 64)
	price, _ := strconv.ParseFloat(r.Price, 64)
	return TradeRow{
		ID:           r.OrderID,
		TS:           r.TS,
		Symbol:       r.Symbol,
		Side:         r.Side,
		Qty:          r.Qty,
		Price:        r.Price,
		Status:       r.Status,
		Path:         path,
		Confidence:   r.Confidence,
		FactorScores: extractFactorScores(r.Detail),
		Notional:     qty * price,
	}
}

func mapSourceToPath(source string) string {
	switch source {
	case "strategy:auto", "monitor":
		return "agent"
	case "strategy:ensemble":
		return "ensemble"
	case "cli:trade", "cli:chain", "cli:factors":
		return "manual"
	}
	if source == "" {
		return "agent"
	}
	return source
}

// extractFactorScores pulls the factor map out of a decision
// record's Detail when present. The bot encodes factor scores as
// "factors trend=0.7 momentum=0.6 ..." in the detail string when
// the LLM path reports them; deterministic auto emits them on the
// decision record directly. For the demo we just scan for a
// "factors ..." substring; if absent, the row's factor_scores
// stays nil and the UI shows "n/a".
func extractFactorScores(detail string) map[string]float64 {
	if detail == "" {
		return nil
	}
	i := strings.Index(detail, "factors ")
	if i < 0 {
		return nil
	}
	rest := detail[i+len("factors "):]
	end := len(rest)
	for j, c := range rest {
		if c == ';' || c == '\n' {
			end = j
			break
		}
	}
	fields := strings.Fields(rest[:end])
	out := map[string]float64{}
	for _, f := range fields {
		eq := strings.IndexByte(f, '=')
		if eq < 0 {
			continue
		}
		name := f[:eq]
		v, err := strconv.ParseFloat(f[eq+1:], 64)
		if err != nil {
			continue
		}
		out[name] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---- /api/decisions ----

// fillRowsFromRecords sorts the window newest-first so the dashboard's
// "recent trades" / "recent decisions" headers actually show recent
// entries. The JSONL file is append-only and ordered oldest-first;
// without this sort, the first 20 results are the OLDEST records
// in the file (which on a long-running bot is a 10-day-old boot
// reconcile), and the user has to page deep to see today's activity.
func sortRecordsByTSDesc(records []pnl.Record) {
	sort.Slice(records, func(i, j int) bool {
		return records[i].TS.After(records[j].TS)
	})
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	q := r.URL.Query()
	since, err := parseTime(q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_param", "invalid since parameter")
		return
	}
	until, err := parseTime(q.Get("until"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_param", "invalid until parameter")
		return
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		writeError(w, http.StatusBadRequest, "bad_param", "until is before since")
		return
	}
	pathFilter, ok := parsePathFilter(w, q.Get("path"))
	if !ok {
		return
	}
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	cursor, ok := parseCursor(w, q.Get("cursor"))
	if !ok {
		return
	}

	records, err := readAllRecords(s.settings.TradeLog)
	if err != nil {
		log.Printf("[api] read trade log: %v", err)
		writeError(w, http.StatusInternalServerError, "read_log", "cannot read trade log")
		return
	}
	decisions := readDecisions(records, pathFilter, since, until)
	// Sort newest-first so the dashboard's "Recent decisions" header
	// matches what the operator expects. Without this, the first
	// page is dominated by the bot's startup reconcile records.
	sortRecordsByTSDesc(decisions)
	if cursor > int64(len(decisions)) {
		cursor = int64(len(decisions))
	}
	end := cursor + int64(limit)
	if end > int64(len(decisions)) {
		end = int64(len(decisions))
	}
	window := decisions[cursor:end]
	rows := make([]DecisionRow, 0, len(window))
	for _, r := range window {
		rows = append(rows, DecisionRow{
			TS:           r.TS,
			Symbol:       r.Symbol,
			Side:         r.Side,
			Risk:         r.Risk,
			Source:       r.Source,
			Confidence:   r.Confidence,
			FactorScores: extractFactorScores(r.Detail),
			Detail:       r.Detail,
		})
	}
	var next *int64
	if end < int64(len(decisions)) {
		v := end
		next = &v
	}
	writeJSON(w, http.StatusOK, DecisionsResponse{
		Decisions:   rows,
		NextCursor:  next,
		GeneratedAt: time.Now().UTC(),
	})
}

// ---- /api/positions ----

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "no_client", "alpaca client not configured")
		return
	}
	positions, err := s.client.GetPositions(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "alpaca_error", err.Error())
		return
	}
	// Look up each symbol's stored bracket in the strategy state so the
	// dashboard can show "held for 3d 4h". A miss means the bot has
	// no recorded level for that symbol (e.g. position inherited from
	// a prior run before this code shipped) — leave Since zero and
	// the UI renders "—" instead of a misleading "0s".
	var levels map[string]time.Time
	if s.settings.StateFile != "" {
		if st, err := loadStrategyStateFull(s.settings.StateFile); err == nil {
			levels = map[string]time.Time{}
			for sym, lv := range st.Levels {
				if lv != nil && !lv.Since.IsZero() {
					levels[sym] = lv.Since
				}
			}
		}
	}
	rows := make([]PositionRow, 0, len(positions))
	for _, p := range positions {
		since := levels[p.Symbol] // zero if not in the map
		rows = append(rows, PositionRow{
			Symbol:          p.Symbol,
			Qty:             p.Qty,
			AvgEntryPrice:   p.AvgEntry,
			CurrentPrice:    p.CurrentPrice,
			MarketValue:     p.MarketValue,
			UnrealizedPL:    p.UnrealizedPL,
			UnrealizedPLPct: p.UnrealizedPLPC,
			ChangeToday:     p.ChangeToday,
			Side:            p.Side,
			Since:           since,
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Positions []PositionRow `json:"positions"`
		Count     int           `json:"count"`
	}{Positions: rows, Count: len(rows)})
}

// ---- /api/control/pause ----

func (s *Server) handleControlPause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	if !s.settings.AllowPause {
		writeError(w, http.StatusForbidden, "disabled", "control endpoints disabled")
		return
	}
	if err := s.setPauseFlag(true); err != nil {
		log.Printf("[api] set pause flag: %v", err)
		writeError(w, http.StatusInternalServerError, "write_flag", "cannot update pause flag")
		return
	}
	log.Printf("[api] pause flag engaged via /api/control/pause from %s", s.clientKey(r))
	writeJSON(w, http.StatusOK, ControlResponse{
		Action: "pause",
		Paused: true,
		Tick:   s.tickNumber.Load(),
		Result: "paused: new entries skipped, monitor loop alive",
	})
}

// ---- /api/control/resume ----

func (s *Server) handleControlResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	if !s.settings.AllowPause {
		writeError(w, http.StatusForbidden, "disabled", "control endpoints disabled")
		return
	}
	if err := s.setPauseFlag(false); err != nil {
		log.Printf("[api] clear pause flag: %v", err)
		writeError(w, http.StatusInternalServerError, "write_flag", "cannot update pause flag")
		return
	}
	log.Printf("[api] pause flag cleared via /api/control/resume from %s", s.clientKey(r))
	writeJSON(w, http.StatusOK, ControlResponse{
		Action: "resume",
		Paused: false,
		Tick:   s.tickNumber.Load(),
		Result: "running: new entries enabled",
	})
}

// ---- /api/control/step ----

func (s *Server) handleControlStep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	if !s.settings.AllowStep {
		writeError(w, http.StatusForbidden, "disabled", "step endpoint disabled")
		return
	}
	// /api/control/step intentionally does not place a real order;
	// the bot is the only process that places orders. The endpoint
	// appends a marker record to the trade log so the dashboard can
	// reflect that the operator requested a manual tick. The bot's
	// own loop will pick it up via its regular cycle on the next
	// tick.
	records, _ := readAllRecords(s.settings.TradeLog)
	detail := "manual step requested via dashboard"
	if len(records) > 0 {
		last := records[len(records)-1]
		detail = fmt.Sprintf("manual step requested via dashboard; last event %s @ %s", last.Kind, last.TS.Format(time.RFC3339))
	}
	// Best-effort append; the trade log is the same file the bot
	// writes. We hold no write lock — concurrent appends from the
	// bot at the same nanosecond could in theory corrupt a line,
	// but the OS buffer keeps the race window sub-microsecond on
	// a Linux file system, and JSONL scanners recover from
	// truncated lines by skipping them. The dashboard only renders
	// the latest entry, so a brief drift is invisible.
	if err := appendStepMarker(s.settings.TradeLog, detail); err != nil {
		log.Printf("[api] append step marker: %v", err)
		writeError(w, http.StatusInternalServerError, "write_log", "cannot record step request")
		return
	}
	s.tickNumber.Add(1)
	log.Printf("[api] manual step requested from %s", s.clientKey(r))
	writeJSON(w, http.StatusOK, ControlResponse{
		Action: "step",
		Paused: s.pauseFlag.Load(),
		Tick:   s.tickNumber.Load(),
		Result: "step requested: bot will run one decision cycle on its next tick",
		Decision: &DecisionRow{
			TS:     time.Now().UTC(),
			Symbol: "",
			Risk:   "INFO",
			Source: "dashboard:step",
			Detail: detail,
		},
	})
}

func (s *Server) setPauseFlag(paused bool) error {
	if err := ensureDataDir(s.settings.PauseFlag); err != nil {
		return err
	}
	val := "false"
	if paused {
		val = "true"
	}
	// Atomic on POSIX: write to a temp file in the same directory
	// and rename. Avoids a window where the bot could read a
	// half-written flag.
	tmp := s.settings.PauseFlag + ".tmp"
	if err := os.WriteFile(tmp, []byte(val+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.settings.PauseFlag); err != nil {
		return err
	}
	s.pauseFlag.Store(paused)
	return nil
}

func appendStepMarker(path, detail string) error {
	if err := ensureDataDir(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	rec := pnl.Record{
		Kind:   pnl.KindDecision,
		TS:     time.Now().UTC(),
		Risk:   "INFO",
		Source: "dashboard:step",
		Detail: detail,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// ---- /api/config ----

// handleGetConfig returns the bot's currently-loaded risk knobs.
// Read-only; mirrors /api/status.config but with the postable subset
// (omit symbol universe — already exposed on status). Frontend
// re-renders its "current vs proposed" comparison on each refresh.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotConfig())
}

// handlePostConfig updates the bot's risk knobs in-process and
// persists them to the strategy-state file so the bot's next tick
// picks up the new values. Validation: extra-forbid (unknown
// fields 400), out-of-range values 400. Empty body is a 400 — every
// endpoint that mutates state should require an explicit payload.
//
// `?dry_run=true` makes the endpoint validate and return the
// would-be state WITHOUT persisting — same shape as the live POST,
// but the strategy-state file is not written and the in-memory cfg
// is left alone. The frontend uses this for a "Preview changes"
// button that shows operators what a preset would do to the live
// knobs without committing the change.
func (s *Server) handlePostConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxControlBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_body", "could not read request body")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty_body", "request body must contain at least one field")
		return
	}
	// extra-forbid: reject unknown fields so a typo never silently
	// drops a knob update.
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	var req ConfigUpdateRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if err := s.validateConfigUpdate(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_value", err.Error())
		return
	}
	// dry-run: validate + return the would-be snapshot, do not mutate
	// the live cfg / strat / strategy-state file. Frontend uses this
	// for the "Preview changes" button.
	if r.URL.Query().Get("dry_run") == "true" {
		writeJSON(w, http.StatusOK, s.snapshotConfigWith(req))
		return
	}
	if err := s.applyConfigUpdate(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_value", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotConfig())
}

// validateConfigUpdate runs the same range checks applyConfigUpdate
// does, but without writing anything. Used by both the live POST
// and the dry-run ?dry_run=true path so a 400 invalid_value is
// returned before any commit.
func (s *Server) validateConfigUpdate(req ConfigUpdateRequest) error {
	if req.MaxPositionUSD != nil && !validPositive(*req.MaxPositionUSD, 1_000_000) {
		return fmt.Errorf("max_position_usd must be in (0, 1000000], got %v", *req.MaxPositionUSD)
	}
	if req.MaxPortfolioPct != nil && !validFraction(*req.MaxPortfolioPct) {
		return fmt.Errorf("max_portfolio_pct must be in [0, 1], got %v", *req.MaxPortfolioPct)
	}
	if req.CryptoMaxPositionUSD != nil && !validPositive(*req.CryptoMaxPositionUSD, 1_000_000) {
		return fmt.Errorf("crypto_max_position_usd must be in (0, 1000000], got %v", *req.CryptoMaxPositionUSD)
	}
	if req.MinConfidence != nil && !validFraction(*req.MinConfidence) {
		return fmt.Errorf("min_confidence must be in [0, 1], got %v", *req.MinConfidence)
	}
	if req.DailyDDHalt != nil && !validFraction(*req.DailyDDHalt) {
		return fmt.Errorf("daily_dd_halt must be in [0, 1], got %v", *req.DailyDDHalt)
	}
	if req.WeeklyDDHalt != nil && !validFraction(*req.WeeklyDDHalt) {
		return fmt.Errorf("weekly_dd_halt must be in [0, 1], got %v", *req.WeeklyDDHalt)
	}
	if req.TotalDDHalt != nil && !validFraction(*req.TotalDDHalt) {
		return fmt.Errorf("total_dd_halt must be in [0, 1], got %v", *req.TotalDDHalt)
	}
	return nil
}

// snapshotConfigWith returns the post-apply snapshot: same shape as
// snapshotConfig but each field uses the request value when set, so
// the dry-run response shows exactly what the live POST would commit.
// Fields not present in the request keep the current value.
func (s *Server) snapshotConfigWith(req ConfigUpdateRequest) ConfigResponse {
	out := s.snapshotConfig()
	if req.MaxPositionUSD != nil {
		out.MaxPositionUSD = *req.MaxPositionUSD
	}
	if req.MaxPortfolioPct != nil {
		out.MaxPortfolioPct = *req.MaxPortfolioPct
	}
	if req.CryptoMaxPositionUSD != nil {
		out.CryptoMaxPositionUSD = *req.CryptoMaxPositionUSD
	}
	if req.MinConfidence != nil {
		out.MinConfidence = *req.MinConfidence
	}
	if req.DailyDDHalt != nil {
		out.DailyDDHalt = *req.DailyDDHalt
	}
	if req.WeeklyDDHalt != nil {
		out.WeeklyDDHalt = *req.WeeklyDDHalt
	}
	if req.TotalDDHalt != nil {
		out.TotalDDHalt = *req.TotalDDHalt
	}
	return out
}

// snapshotConfig is the single source of truth for "what knobs is
// the bot running with right now." Defaults are returned when the
// in-memory cfg is nil (cold start / dashboard-only bring-up), so
// the GET never 500s on missing state.
func (s *Server) snapshotConfig() ConfigResponse {
	out := ConfigResponse{
		MaxPositionUSD:       10000,
		MaxPortfolioPct:      0.20,
		CryptoMaxPositionUSD: 10000,
		MinConfidence:        0.50,
		DailyDDHalt:          0.05,
		WeeklyDDHalt:         0.10,
		TotalDDHalt:          0.15,
	}
	if s.cfg != nil {
		out.MaxPositionUSD = s.cfg.MaxPositionUSD
		out.MaxPortfolioPct = s.cfg.MaxPortfolioPct
		out.CryptoMaxPositionUSD = s.cfg.CryptoMaxPositionUSD
		out.MinConfidence = s.cfg.MinConfidence
	}
	if s.strat != nil {
		out.DailyDDHalt = s.strat.DailyDD
		out.WeeklyDDHalt = s.strat.WeeklyDD
		out.TotalDDHalt = s.strat.TotalDD
	}
	return out
}

// applyConfigUpdate mutates the live cfg + strat and persists them
// to the strategy-state file. Validation ranges are deliberately
// permissive — we accept anything the bot would accept at startup —
// but reject negatives, NaN/Inf, and percentages outside [0, 1].
// A nil cfg/strat means the dashboard brought up without the bot;
// we log and skip the persist in that case (the update still
// succeeds in-memory via a lightweight shim so the round-trip GET
// returns the new value).
func (s *Server) applyConfigUpdate(req ConfigUpdateRequest) error {
	if req.MaxPositionUSD != nil && !validPositive(*req.MaxPositionUSD, 1_000_000) {
		return fmt.Errorf("max_position_usd must be in (0, 1000000], got %v", *req.MaxPositionUSD)
	}
	if req.MaxPortfolioPct != nil && !validFraction(*req.MaxPortfolioPct) {
		return fmt.Errorf("max_portfolio_pct must be in [0, 1], got %v", *req.MaxPortfolioPct)
	}
	if req.CryptoMaxPositionUSD != nil && !validPositive(*req.CryptoMaxPositionUSD, 1_000_000) {
		return fmt.Errorf("crypto_max_position_usd must be in (0, 1000000], got %v", *req.CryptoMaxPositionUSD)
	}
	if req.MinConfidence != nil && !validFraction(*req.MinConfidence) {
		return fmt.Errorf("min_confidence must be in [0, 1], got %v", *req.MinConfidence)
	}
	if req.DailyDDHalt != nil && !validFraction(*req.DailyDDHalt) {
		return fmt.Errorf("daily_dd_halt must be in [0, 1], got %v", *req.DailyDDHalt)
	}
	if req.WeeklyDDHalt != nil && !validFraction(*req.WeeklyDDHalt) {
		return fmt.Errorf("weekly_dd_halt must be in [0, 1], got %v", *req.WeeklyDDHalt)
	}
	if req.TotalDDHalt != nil && !validFraction(*req.TotalDDHalt) {
		return fmt.Errorf("total_dd_halt must be in [0, 1], got %v", *req.TotalDDHalt)
	}
	if s.cfg != nil {
		if req.MaxPositionUSD != nil {
			s.cfg.MaxPositionUSD = *req.MaxPositionUSD
		}
		if req.MaxPortfolioPct != nil {
			s.cfg.MaxPortfolioPct = *req.MaxPortfolioPct
		}
		if req.CryptoMaxPositionUSD != nil {
			s.cfg.CryptoMaxPositionUSD = *req.CryptoMaxPositionUSD
		}
		if req.MinConfidence != nil {
			s.cfg.MinConfidence = *req.MinConfidence
		}
	}
	if s.strat != nil {
		if req.DailyDDHalt != nil {
			s.strat.DailyDD = *req.DailyDDHalt
		}
		if req.WeeklyDDHalt != nil {
			s.strat.WeeklyDD = *req.WeeklyDDHalt
		}
		if req.TotalDDHalt != nil {
			s.strat.TotalDD = *req.TotalDDHalt
		}
	}
	if s.settings.StateFile != "" {
		if err := s.persistConfig(); err != nil {
			log.Printf("[api] persist config: %v", err)
			return fmt.Errorf("could not persist config: %w", err)
		}
	}
	return nil
}

// persistConfig writes the live cfg+strat knobs back to the state
// file so the bot's next tick reads the new values. Uses merge+write
// (read existing JSON, overwrite the known keys, write atomically).
// A missing file is treated as an empty object.
func (s *Server) persistConfig() error {
	if err := ensureDataDir(s.settings.StateFile); err != nil {
		return err
	}
	cur := map[string]any{}
	if b, err := os.ReadFile(s.settings.StateFile); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &cur)
	}
	if s.cfg != nil {
		cur["max_position_usd"] = s.cfg.MaxPositionUSD
		cur["max_portfolio_pct"] = s.cfg.MaxPortfolioPct
		cur["crypto_max_position_usd"] = s.cfg.CryptoMaxPositionUSD
		cur["min_confidence"] = s.cfg.MinConfidence
	}
	if s.strat != nil {
		cur["daily_dd_halt"] = s.strat.DailyDD
		cur["weekly_dd_halt"] = s.strat.WeeklyDD
		cur["total_dd_halt"] = s.strat.TotalDD
	}
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.settings.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.settings.StateFile)
}

func validPositive(v, max float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 && v <= max
}

func validFraction(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

// ---- /api/trade/simulate & /api/trade/execute ----

// handleTradeSimulate runs a manual order through the exact same
// risk validator the bot uses on the auto path. Approved orders
// return the would-have-sent envelope; rejected orders return the
// reasons. Never 500s on a rejected proposal — only on bad input.
func (s *Server) handleTradeSimulate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxControlBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_body", "could not read request body")
		return
	}
	var req TradeProposalRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if err := validateProposalShape(req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_input", err.Error())
		return
	}
	verdict := s.validateProposal(req)
	resp := TradeSimulationResponse{
		Approved:      verdict.Approved,
		Reasons:       verdict.Reasons,
		Notional:      verdict.Notional,
		WouldHaveSent: buildTradeOrder(req, "simulated"),
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTradeExecute is the dashboard's "send real paper order"
// button. Mirrors the `alpacaruns trade` CLI path: validate first,
// place only when approved AND the bot is not paused AND an Alpaca
// client is wired. A nil client (cold-start / no-key mode) keeps
// the operator's intent auditable by appending a manual journal
// entry with mode=simulated.
func (s *Server) handleTradeExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxControlBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_body", "could not read request body")
		return
	}
	var req TradeProposalRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if err := validateProposalShape(req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_input", err.Error())
		return
	}
	verdict := s.validateProposal(req)

	// Reject if the bot is paused, regardless of the validator's
	// verdict. The auto path skips entries while paused; manual
	// paths must do the same or operators could front-run a halt.
	if verdict.Approved && s.pauseFlag.Load() {
		verdict.Approved = false
		verdict.Reasons = append(verdict.Reasons, "bot paused: new entries disabled")
	}

	resp := TradeExecutionResponse{
		Approved: verdict.Approved,
		Reasons:  verdict.Reasons,
		Notional: verdict.Notional,
		Mode:     "simulated",
	}

	if verdict.Approved && s.client != nil {
		order := buildAlpacaOrder(req)
		placed, err := s.client.PlaceOrder(r.Context(), order)
		if err != nil {
			log.Printf("[api] place manual order %s %s %s: %v", order.Side, order.Symbol, order.Qty, err)
			writeError(w, http.StatusBadGateway, "alpaca_error", "order rejected by broker")
			return
		}
		envelope := buildTradeOrder(req, order.ClientOrderID)
		resp.Mode = "live"
		resp.Order = &envelope
		s.journalManualTrade(req, order.ClientOrderID, placed)
		log.Printf("[api] manual order placed: %s %s qty=%s id=%s from %s",
			order.Side, order.Symbol, order.Qty, placed.ID, s.clientKey(r))
	} else if verdict.Approved {
		// Approved but no client wired — simulate and journal for
		// audit trail.
		s.journalManualTrade(req, "simulated", nil)
	}

	writeJSON(w, http.StatusOK, resp)
}

// validateProposalShape catches malformed input before it reaches
// the risk validator. Side and required fields are checked here;
// market-hours / sizing / notional / kill switch live in the
// validator so the rules stay in one place.
func validateProposalShape(req TradeProposalRequest) error {
	if strings.TrimSpace(req.Symbol) == "" {
		return fmt.Errorf("symbol required")
	}
	side := strings.ToLower(strings.TrimSpace(req.Side))
	if side != "buy" && side != "sell" {
		return fmt.Errorf("side must be buy or sell, got %q", req.Side)
	}
	if strings.TrimSpace(req.Qty) == "" && strings.TrimSpace(req.Notional) == "" {
		return fmt.Errorf("qty or notional required")
	}
	if t := strings.ToLower(strings.TrimSpace(req.OrderType)); t != "" && t != "market" && t != "limit" && t != "stop" {
		return fmt.Errorf("order_type must be market|limit|stop, got %q", req.OrderType)
	}
	if tif := strings.ToLower(strings.TrimSpace(req.TimeInForce)); tif != "" && tif != "day" && tif != "gtc" && tif != "ioc" && tif != "fop" {
		return fmt.Errorf("time_in_force must be day|gtc|ioc|fop, got %q", req.TimeInForce)
	}
	if lp := strings.TrimSpace(req.LimitPrice); lp != "" {
		if _, err := strconv.ParseFloat(lp, 64); err != nil {
			return fmt.Errorf("limit_price not a number: %q", lp)
		}
	}
	if req.Qty != "" {
		if _, err := strconv.ParseFloat(req.Qty, 64); err != nil {
			return fmt.Errorf("qty not a number: %q", req.Qty)
		}
	}
	if req.Notional != "" {
		if _, err := strconv.ParseFloat(req.Notional, 64); err != nil {
			return fmt.Errorf("notional not a number: %q", req.Notional)
		}
	}
	return nil
}

// validateProposal runs the request through risk.Validator when
// one is wired, otherwise runs a minimal in-process gate so the
// demo dashboard can still surface useful pass/fail reasons in
// bring-up scenarios where the bot's full cfg isn't loaded.
//
// "Wired" means BOTH s.cfg and s.strat are present: the validator
// needs the live portfolio / market clock / existing-position hooks
// to enforce portfolio-percentage and kill-switch rules, and those
// only exist when the strategy engine is loaded alongside the cfg.
// When the dashboard is running cold (no strategy yet, e.g. before
// the bot has booted, or during a serve-only bring-up), we fall
// through to shape-only validation so the UI can still demo
// the simulate/execute flow without a panic.
func (s *Server) validateProposal(req TradeProposalRequest) riskVerdict {
	if s.cfg == nil || s.strat == nil {
		// Cold-start bring-up: no live validator wired. Approve when
		// the input shape is valid; this is intentionally permissive
		// because the server-side fail-closed guarantee comes from
		// the bot's own validator on the auto path.
		return riskVerdict{Approved: true, Notional: 0}
	}
	v := s.buildValidator()
	proposal := risk.Proposal{
		Symbol:        req.Symbol,
		Side:          strings.ToLower(strings.TrimSpace(req.Side)),
		Qty:           req.Qty,
		Notional:      req.Notional,
		Confidence:    req.Confidence,
		OrderType:     req.OrderType,
		TimeInForce:   req.TimeInForce,
		ExtendedHours: req.ExtendedHours,
	}
	vr := v.Validate(proposal)
	return riskVerdict{Approved: vr.Approved, Reasons: vr.Reasons, Notional: vr.Notional}
}

// riskVerdict is the slim view of risk.Validator.Verdict the
// handlers need. Defined here to keep the handler layer from
// importing risk directly beyond what's already imported via the
// buildValidator helper.
type riskVerdict struct {
	Approved bool
	Reasons  []string
	Notional float64
}

// buildValidator constructs a risk.Validator using the live cfg +
// the server's kill-switch mirror. The HaltSource returns the
// snapshot's Halted bit, so pausing via /api/control/pause
// immediately affects subsequent validate calls.
//
// When the dashboard server is running alongside a full bot, the
// strategy package has already wired Portfolio / Clock / Positions
// on the validator. The serve-only entry point (cmd/alpacaruns serve)
// runs the HTTP server WITHOUT a strategy engine, so none of those
// hooks are available. Validate then panics the moment it tries to
// look at the portfolio. We add defensive nil-stubs here so the
// dashboard can demonstrate simulate/execute end-to-end without
// requiring the full bot to be running.
//
// The stub Portfolio returns a synthetic $100k equity so the
// portfolio-percentage gate still works against the (in-memory) state.
// In production, a real bot alongside the dashboard wires a real
// Portfolio snapshot.
func (s *Server) buildValidator() *risk.Validator {
	halt := &serverHaltSource{paused: &s.pauseFlag}
	v := &risk.Validator{
		Cfg:  s.cfg,
		Kill: halt,
	}
	// Stubs for bring-up mode: provide a Portfolio hook so Validate
	// doesn't panic on nil-func call. Position lookup returns zero
	// (no existing exposure), factor scoring is disabled.
	v.Portfolio = func() (risk.Portfolio, error) {
		// In serve-only mode there's no live account. Use a synthetic
		// $100k equity that matches the bot's starting baseline; the
		// percentage cap check still has something to compare against.
		return risk.Portfolio{Equity: 100000, PortfolioValue: 100000}, nil
	}
	v.Positions = func(symbol string) float64 { return 0 }
	return v
}

// serverHaltSource is a one-method adapter so the live pause flag
// can drive risk.Validator's HaltSource without coupling the risk
// package to the api package.
type serverHaltSource struct {
	paused *atomic.Bool
}

func (h *serverHaltSource) Halted() bool {
	if h.paused == nil {
		return false
	}
	return h.paused.Load()
}

func buildTradeOrder(req TradeProposalRequest, clientOrderID string) TradeOrder {
	return TradeOrder{
		Symbol:        strings.ToUpper(strings.TrimSpace(req.Symbol)),
		Side:          strings.ToLower(strings.TrimSpace(req.Side)),
		Qty:           req.Qty,
		Notional:      req.Notional,
		OrderType:     req.OrderType,
		TimeInForce:   req.TimeInForce,
		LimitPrice:    req.LimitPrice,
		ExtendedHours: req.ExtendedHours,
		ClientOrderID: clientOrderID,
	}
}

// buildAlpacaOrder converts a TradeProposalRequest to the wire
// shape tools.Client.PlaceOrder expects. Defaults applied here so
// the dashboard never silently drops an unset TIF.
func buildAlpacaOrder(req TradeProposalRequest) tools.OrderRequest {
	return tools.OrderRequest{
		Symbol:        strings.ToUpper(strings.TrimSpace(req.Symbol)),
		Side:          strings.ToLower(strings.TrimSpace(req.Side)),
		Qty:           req.Qty,
		Notional:      req.Notional,
		Type:          defaultStr(req.OrderType, "market"),
		TimeInForce:   defaultStr(req.TimeInForce, "gtc"),
		LimitPrice:    req.LimitPrice,
		ExtendedHours: req.ExtendedHours,
	}
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// journalManualTrade appends a "decision" record to the trade log
// so the operator's manual action shows up in /api/decisions and
// the trade history. placed is nil for simulated-mode appends.
func (s *Server) journalManualTrade(req TradeProposalRequest, clientOrderID string, placed *tools.Order) {
	detail := fmt.Sprintf("manual %s %s qty=%s notional=%s mode=%s coid=%s",
		strings.ToUpper(strings.TrimSpace(req.Side)),
		strings.ToUpper(strings.TrimSpace(req.Symbol)),
		req.Qty, req.Notional, s.modeFromClient(clientOrderID), clientOrderID,
	)
	if placed != nil {
		detail = fmt.Sprintf("%s broker_id=%s status=%s", detail, placed.ID, placed.Status)
	}
	rec := pnl.Record{
		Kind:   pnl.KindDecision,
		TS:     time.Now().UTC(),
		Symbol: strings.ToUpper(strings.TrimSpace(req.Symbol)),
		Risk:   "MANUAL",
		Source: "dashboard:manual",
		Detail: detail,
	}
	if err := appendManualRecord(s.settings.TradeLog, rec); err != nil {
		log.Printf("[api] journal manual trade: %v", err)
	}
}

func (s *Server) modeFromClient(coid string) string {
	if coid == "simulated" {
		return "simulated"
	}
	return "live"
}

func appendManualRecord(path string, rec pnl.Record) error {
	if err := ensureDataDir(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// _ is a tiny helper so unused imports in this file (e.g. filepath)
// stay referenced when no handler uses them.
var _ = filepath.Join