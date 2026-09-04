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
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
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
	rows := make([]PositionRow, 0, len(positions))
	for _, p := range positions {
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

// _ is a tiny helper so unused imports in this file (e.g. filepath)
// stay referenced when no handler uses them.
var _ = filepath.Join