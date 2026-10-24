// Package store is the queryable, transactional view of the trading facts.
//
// The JSON and JSONL files remain the WRITE path: one writer per file, atomic
// rename, append-only journal. That is deliberate. This package is the
// migration stage the files were standing in for — it mirrors the same facts
// into SQLite so they can be queried, joined and indexed, and it proves the
// mirror agrees with the sources before anyone reads from it.
//
// Why a mirror rather than a cut-over: the live monitor depends on those
// files every tick. Swapping the store under it in one commit would put
// position protection on the critical path of a schema change. Instead:
//
//	sources (authoritative) --ingest--> SQLite (derived, verified)
//
//	Verify() replays both sides and reports every disagreement. A reader may
//	only be pointed at SQLite when Verify() is clean, and the files stay the
//	recovery path if the database is ever lost.
//
// Every write is an upsert on a natural key, so ingestion is idempotent:
// re-ingesting the whole journal appends nothing.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the static cross-compile working

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
)

// schemaVersion is stored in meta and checked on open. Migrations are applied
// in order and recorded, so a file written by an older binary is upgraded
// rather than rejected.
const schemaVersion = 1

// DB is one open mirror.
type DB struct {
	sql  *sql.DB
	path string
}

// Open creates or opens the mirror at path and applies migrations.
func Open(path string) (*DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("store: empty path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create dir: %w", err)
		}
	}
	// _pragma=busy_timeout makes concurrent readers wait instead of failing
	// with SQLITE_BUSY: the API and the bot can both have the file open.
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// One connection: SQLite serialises writers anyway, and a pool would only
	// add lock contention inside this process.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	d := &DB{sql: db, path: path}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// Path returns the backing file.
func (d *DB) Path() string { return d.path }

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		// Fills are keyed by (order, seq): one order can fill in several
		// prints, and the journal records that as separate rows.
		// The natural key is the fill itself: order id, print timestamp,
		// quantity and price. A derived sequence looked tidier and was wrong —
		// on a re-ingest it started from the row count and minted new rows, so
		// the mirror grew on every tick. Two fills agreeing on all four fields
		// are the same print.
		`CREATE TABLE IF NOT EXISTS fills (
			order_id TEXT NOT NULL,
			client_order_id TEXT,
			symbol TEXT NOT NULL,
			side TEXT NOT NULL,
			qty REAL NOT NULL,
			price REAL NOT NULL,
			status TEXT,
			ts TEXT NOT NULL,
			PRIMARY KEY (order_id, ts, qty, price)
		)`,
		`CREATE TABLE IF NOT EXISTS intents (
			client_order_id TEXT PRIMARY KEY,
			symbol TEXT,
			side TEXT,
			qty REAL,
			price REAL,
			score REAL,
			ts TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			symbol TEXT,
			side TEXT,
			source TEXT,
			risk TEXT,
			confidence REAL,
			detail TEXT,
			UNIQUE (ts, symbol, source, detail)
		)`,
		`CREATE TABLE IF NOT EXISTS levels (
			symbol TEXT NOT NULL,
			track TEXT NOT NULL,
			entry_price REAL,
			take_profit REAL,
			stop_loss REAL,
			qty REAL,
			side TEXT,
			asset_class TEXT,
			since TEXT,
			PRIMARY KEY (symbol, track)
		)`,
		`CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS fills_symbol_ts ON fills(symbol, ts)`,
		`CREATE INDEX IF NOT EXISTS decisions_ts ON decisions(ts)`,
	}
	for _, s := range stmts {
		if _, err := d.sql.Exec(s); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	var got int
	err := d.sql.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		if _, err := d.sql.Exec(`INSERT INTO meta(key, value) VALUES('schema_version', ?)`, strconv.Itoa(schemaVersion)); err != nil {
			return fmt.Errorf("store: stamp schema: %w", err)
		}
	case err != nil:
		return fmt.Errorf("store: read schema version: %w", err)
	case got > schemaVersion:
		return fmt.Errorf("store: %s has schema v%d, this build understands v%d — refusing to write over a newer file", d.path, got, schemaVersion)
	}
	return nil
}

// Ingest mirrors journal records. It is idempotent: rows are upserted on
// their natural key, so the same journal can be ingested on every tick.
func (d *DB) Ingest(recs []pnl.Record) (int, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	written := 0
	for _, r := range recs {
		switch r.Kind {
		case pnl.KindFill:
			order := strings.TrimSpace(r.OrderID)
			qty, e1 := strconv.ParseFloat(strings.TrimSpace(r.Qty), 64)
			price, e2 := strconv.ParseFloat(strings.TrimSpace(r.Price), 64)
			if order == "" || e1 != nil || e2 != nil {
				continue // unattributable rows stay in the journal; they are not queryable facts
			}
			res, err := tx.Exec(`INSERT OR IGNORE INTO fills(order_id, client_order_id, symbol, side, qty, price, status, ts)
				VALUES(?,?,?,?,?,?,?,?)`,
				order, r.ClientOrderID, strings.TrimSpace(r.Symbol), strings.ToLower(strings.TrimSpace(r.Side)),
				qty, price, r.Status, r.TS.UTC().Format(time.RFC3339Nano))
			if err != nil {
				return written, fmt.Errorf("store: ingest fill %s: %w", order, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				written++
			}
		case pnl.KindIntent:
			if strings.TrimSpace(r.ClientOrderID) == "" {
				continue
			}
			var score any
			if r.Confidence != nil {
				score = *r.Confidence
			}
			qty, _ := strconv.ParseFloat(strings.TrimSpace(r.Qty), 64)
			price, _ := strconv.ParseFloat(strings.TrimSpace(r.Price), 64)
			res, err := tx.Exec(`INSERT INTO intents(client_order_id, symbol, side, qty, price, score, ts)
				VALUES(?,?,?,?,?,?,?)
				ON CONFLICT(client_order_id) DO NOTHING`,
				r.ClientOrderID, strings.TrimSpace(r.Symbol), strings.ToLower(strings.TrimSpace(r.Side)),
				qty, price, score, r.TS.UTC().Format(time.RFC3339Nano))
			if err != nil {
				return written, fmt.Errorf("store: ingest intent %s: %w", r.ClientOrderID, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				written++
			}
		case pnl.KindDecision, pnl.KindReconcile:
			var conf any
			if r.Confidence != nil {
				conf = *r.Confidence
			}
			res, err := tx.Exec(`INSERT INTO decisions(ts, symbol, side, source, risk, confidence, detail)
				VALUES(?,?,?,?,?,?,?)
				ON CONFLICT(ts, symbol, source, detail) DO NOTHING`,
				r.TS.UTC().Format(time.RFC3339Nano), strings.TrimSpace(r.Symbol),
				strings.ToLower(strings.TrimSpace(r.Side)), r.Source, r.Risk, conf, r.Detail)
			if err != nil {
				return written, fmt.Errorf("store: ingest decision: %w", err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				written++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return written, fmt.Errorf("store: commit: %w", err)
	}
	return written, nil
}

// MirrorState replaces the levels table with the state document's levels and
// stores its scalars in kv. Levels are replaced rather than merged: the state
// file is the set, and a level removed there has been closed.
func (d *DB) MirrorState(st *strategy.State) error {
	if st == nil {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM levels`); err != nil {
		return fmt.Errorf("store: clear levels: %w", err)
	}
	keys := make([]string, 0, len(st.Levels))
	for k := range st.Levels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lv := st.Levels[k]
		if lv == nil {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO levels(symbol, track, entry_price, take_profit, stop_loss, qty, side, asset_class, since)
			VALUES(?,?,?,?,?,?,?,?,?)`,
			lv.Symbol, lv.Track, lv.EntryPrice, lv.TakeProfit, lv.StopLoss,
			lv.Qty, lv.Side, lv.AssetClass, lv.Since.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("store: mirror level %s: %w", lv.Symbol, err)
		}
	}
	kv := map[string]string{
		"version":         strconv.Itoa(st.Version),
		"peak_equity":     strconv.FormatFloat(st.PeakEquity, 'f', -1, 64),
		"week_start":      st.WeekStart.UTC().Format(time.RFC3339Nano),
		"tick_number":     strconv.FormatInt(st.TickNumber, 10),
		"last_tick":       st.LastTick.UTC().Format(time.RFC3339Nano),
		"starting_equity": strconv.FormatFloat(st.StartingEquity, 'f', -1, 64),
	}
	for k, v := range kv {
		if _, err := tx.Exec(`INSERT INTO kv(key, value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			return fmt.Errorf("store: mirror kv %s: %w", k, err)
		}
	}
	return tx.Commit()
}

// Verify is the shadow-read comparison: it replays the sources and asks the
// mirror whether it agrees. A reader may be pointed at SQLite only when this
// reports clean — "the database is fine" is a claim that has to be checked,
// not assumed.
type VerifyReport struct {
	OK         bool     `json:"ok"`
	Mismatches []string `json:"mismatches,omitempty"`
	Fills      int      `json:"fills"`
	Intents    int      `json:"intents"`
	Decisions  int      `json:"decisions"`
	Levels     int      `json:"levels"`
}

func (r VerifyReport) Summary() string {
	if r.OK {
		return fmt.Sprintf("mirror verified: %d fills, %d intents, %d decisions, %d levels",
			r.Fills, r.Intents, r.Decisions, r.Levels)
	}
	return fmt.Sprintf("mirror DOES NOT MATCH the sources (%d mismatch(es)): %s",
		len(r.Mismatches), strings.Join(r.Mismatches, "; "))
}

// Verify compares the mirror against the journal and the state document.
func (d *DB) Verify(recs []pnl.Record, st *strategy.State) (VerifyReport, error) {
	rep := VerifyReport{OK: true}

	// --- fills: count and quantity per order id ---
	wantQty := map[string]float64{}
	wantFills := 0
	for _, r := range recs {
		if r.Kind != pnl.KindFill {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(r.Qty), 64)
		if err != nil || strings.TrimSpace(r.OrderID) == "" {
			continue
		}
		wantFills++
		wantQty[r.OrderID] += q
	}
	gotQty := map[string]float64{}
	rows, err := d.sql.Query(`SELECT order_id, qty FROM fills`)
	if err != nil {
		return rep, fmt.Errorf("store: verify fills: %w", err)
	}
	gotFills := 0
	for rows.Next() {
		var id string
		var q float64
		if err := rows.Scan(&id, &q); err == nil {
			gotFills++
			gotQty[id] += q
		}
	}
	rows.Close()
	rep.Fills = gotFills
	if gotFills != wantFills {
		rep.OK = false
		rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("fill rows: mirror %d, journal %d", gotFills, wantFills))
	}
	for id, want := range wantQty {
		if diff := gotQty[id] - want; diff > 1e-9 || diff < -1e-9 {
			rep.OK = false
			rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("order %s qty: mirror %.8f, journal %.8f", id, gotQty[id], want))
		}
	}

	// --- intents ---
	// Compared as a SET of ids: the journal can hold two intent rows for one
	// client_order_id (an attempt and its resolution), while the mirror keys
	// on the id itself. Comparing raw counts would report a false mismatch.
	wantIDs := map[string]struct{}{}
	for _, r := range recs {
		if r.Kind == pnl.KindIntent && strings.TrimSpace(r.ClientOrderID) != "" {
			wantIDs[strings.TrimSpace(r.ClientOrderID)] = struct{}{}
		}
	}
	gotIDs := map[string]struct{}{}
	if rows, err := d.sql.Query(`SELECT client_order_id FROM intents`); err == nil {
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				gotIDs[id] = struct{}{}
			}
		}
		rows.Close()
	}
	rep.Intents = len(gotIDs)
	for id := range wantIDs {
		if _, ok := gotIDs[id]; !ok {
			rep.OK = false
			rep.Mismatches = append(rep.Mismatches, "intent missing in mirror: "+id)
		}
	}

	// --- decisions ---
	wantDec := 0
	for _, r := range recs {
		if r.Kind == pnl.KindDecision || r.Kind == pnl.KindReconcile {
			wantDec++
		}
	}
	rep.Decisions = count(d, `SELECT COUNT(*) FROM decisions`)
	if rep.Decisions != wantDec {
		rep.OK = false
		rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("decisions: mirror %d, journal %d", rep.Decisions, wantDec))
	}

	// --- levels: the set and the two numbers that must never drift ---
	rep.Levels = count(d, `SELECT COUNT(*) FROM levels`)
	if st != nil {
		if rep.Levels != len(st.Levels) {
			rep.OK = false
			rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("levels: mirror %d, state %d", rep.Levels, len(st.Levels)))
		}
		for k, lv := range st.Levels {
			if lv == nil {
				continue
			}
			var tp, sl float64
			err := d.sql.QueryRow(`SELECT take_profit, stop_loss FROM levels WHERE symbol=? AND track=?`,
				lv.Symbol, lv.Track).Scan(&tp, &sl)
			if err == sql.ErrNoRows {
				rep.OK = false
				rep.Mismatches = append(rep.Mismatches, "level missing in mirror: "+k)
				continue
			}
			if err != nil {
				return rep, fmt.Errorf("store: verify level %s: %w", k, err)
			}
			if diff := tp - lv.TakeProfit; diff > 1e-9 || diff < -1e-9 {
				rep.OK = false
				rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("level %s take-profit: mirror %.4f, state %.4f", k, tp, lv.TakeProfit))
			}
			if diff := sl - lv.StopLoss; diff > 1e-9 || diff < -1e-9 {
				rep.OK = false
				rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("level %s stop-loss: mirror %.4f, state %.4f", k, sl, lv.StopLoss))
			}
		}
	}
	return rep, nil
}

func count(d *DB, q string) int {
	var n int
	_ = d.sql.QueryRow(q).Scan(&n)
	return n
}

// FillTotals returns the quantity filled per order id, from the mirror. This
// is the shape the reconciliation pass uses, and the reason the mirror exists:
// a joinable, indexed answer instead of a full journal scan.
func (d *DB) FillTotals() (map[string]float64, error) {
	rows, err := d.sql.Query(`SELECT order_id, SUM(qty) FROM fills GROUP BY order_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var id string
		var q float64
		if err := rows.Scan(&id, &q); err == nil {
			out[id] = q
		}
	}
	return out, rows.Err()
}

// RealizedBySymbol sums per-symbol realised P/L from the mirror's fills, using
// the same FIFO the journal path uses. It exists so a reader can be moved over
// without reimplementing the maths.
func (d *DB) RealizedBySymbol() (map[string]float64, error) {
	rows, err := d.sql.Query(`SELECT order_id, symbol, side, qty, price, ts FROM fills ORDER BY ts, order_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var recs []pnl.Record
	for rows.Next() {
		var id, sym, side, ts string
		var q, p float64
		if err := rows.Scan(&id, &sym, &side, &q, &p, &ts); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, ts)
		recs = append(recs, pnl.Record{Kind: pnl.KindFill, OrderID: id, Symbol: sym, Side: side,
			Qty:   strconv.FormatFloat(q, 'f', -1, 64),
			Price: strconv.FormatFloat(p, 'f', -1, 64), TS: t})
	}
	st := pnl.Compute(recs)
	out := map[string]float64{}
	for _, s := range st.Symbols {
		out[s.Symbol] = s.Realized
	}
	return out, nil
}
