package main

// mirror.go: the SQLite stage.
//
// The JSON/JSONL files stay the write path (one writer each, atomic rename,
// append-only journal); this mirrors the same facts into SQLite and proves
// the mirror agrees before anything reads from it. See package store for why
// this is a mirror and not a cut-over.

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/config"
	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/store"
	"github.com/BROCKUGANDA/alpacaruns/strategy"
)

// mirrorVerifyEvery bounds how often the full shadow comparison runs in the
// bot. Ingestion is per tick (cheap, idempotent); verification replays both
// sides and does not need to run 288 times a day.
const mirrorVerifyEvery = 10

// MirrorPath derives the database path from the journal path so no new flag
// is required: data/trades.jsonl -> data/alpacaruns.db.
func MirrorPath(journalPath string) string {
	dir := filepath.Dir(journalPath)
	if dir == "" || dir == "." {
		return "alpacaruns.db"
	}
	return filepath.Join(dir, "alpacaruns.db")
}

// mirrorOnce ingests the journal and the state document, then verifies.
// It is the whole CLI command and the bot's periodic check.
func mirrorOnce(dbPath, journalPath string) (store.VerifyReport, error) {
	j, err := pnl.Open(journalPath)
	if err != nil {
		return store.VerifyReport{}, fmt.Errorf("journal: %w", err)
	}
	defer j.Close()
	recs, err := j.Records()
	if err != nil {
		return store.VerifyReport{}, fmt.Errorf("read journal: %w", err)
	}
	st := strategy.NewStateStore(journalPath)
	state, err := st.Load()
	if err != nil {
		return store.VerifyReport{}, fmt.Errorf("read state: %w", err)
	}

	d, err := store.Open(dbPath)
	if err != nil {
		return store.VerifyReport{}, err
	}
	defer d.Close()
	if _, err := d.Ingest(recs); err != nil {
		return store.VerifyReport{}, err
	}
	if err := d.MirrorState(state); err != nil {
		return store.VerifyReport{}, err
	}
	return d.Verify(recs, state)
}

// cmdMirror runs the migration stage once and reports whether the mirror
// agrees with the sources. Exit code 1 means it does not: a mirror nobody
// checked is worse than no mirror.
func cmdMirror(args []string) int {
	fs := flag.NewFlagSet("mirror", flag.ContinueOnError)
	envFile := fs.String("env", ".env", "path to .env file")
	dbPath := fs.String("db", "", "database path (default: data/alpacaruns.db)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*envFile)
	if err != nil {
		log.Printf("config: %v", err)
		return 1
	}
	path := strings.TrimSpace(*dbPath)
	if path == "" {
		path = MirrorPath(cfg.TradeLog)
	}
	rep, err := mirrorOnce(path, cfg.TradeLog)
	if err != nil {
		log.Printf("[mirror] %v", err)
		return 1
	}
	log.Printf("[mirror] %s", rep.Summary())
	fmt.Println(rep.Summary())
	if !rep.OK {
		return 1
	}
	return 0
}

// mirrorTick is the bot's side: ingest this tick's journal and state, and run
// the full comparison on a cadence. A failing verification is loud and
// journalled — the mirror must never silently diverge from the book.
func (l *autoLoop) mirrorTick(ctx context.Context) {
	if !l.cfg.MirrorEnabled || l.journal == nil || l.exec == nil || l.exec.State == nil {
		return
	}
	if l.mirror == nil {
		db, err := store.Open(MirrorPath(l.journal.Path()))
		if err != nil {
			log.Printf("[mirror] disabled: %v", err)
			return
		}
		l.mirror = db
	}
	recs, err := l.journal.Records()
	if err != nil {
		log.Printf("[mirror] journal unreadable: %v", err)
		return
	}
	if _, err := l.mirror.Ingest(recs); err != nil {
		log.Printf("[mirror] ingest failed: %v", err)
		return
	}
	st, err := l.exec.State.Load()
	if err != nil {
		log.Printf("[mirror] state unreadable: %v", err)
		return
	}
	if err := l.mirror.MirrorState(st); err != nil {
		log.Printf("[mirror] state mirror failed: %v", err)
		return
	}
	if l.tickNum%mirrorVerifyEvery != 1 {
		return
	}
	rep, err := l.mirror.Verify(recs, st)
	if err != nil {
		log.Printf("[mirror] verify failed: %v", err)
		return
	}
	if !rep.OK {
		log.Printf("[mirror] %s", rep.Summary())
		_ = l.journal.Append(pnl.Record{
			Kind: pnl.KindReconcile, Source: "mirror:verify", Risk: "HEAL_REQUIRED",
			Detail: "SQLite mirror diverged from the sources: " + strings.Join(rep.Mismatches, "; "),
			TS:     time.Now().UTC(),
		})
		return
	}
	log.Printf("[mirror] %s", rep.Summary())
}

// closeMirror releases the database handle on shutdown.
func (l *autoLoop) closeMirror() {
	if l != nil && l.mirror != nil {
		_ = l.mirror.Close()
		l.mirror = nil
	}
}
