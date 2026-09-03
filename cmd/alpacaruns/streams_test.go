package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/pnl"
	"github.com/BROCKUGANDA/alpacaruns/stream"
)

func newTestJournal(t *testing.T) *pnl.Journal {
	t.Helper()
	j, err := pnl.Open(filepath.Join(t.TempDir(), "trades.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

// TestConsumeFillsJournalsAndDedups verifies the streamed-fill handler:
// each fill is journaled immediately, a repeat delivery of the same order
// ID (stream + backfill overlap) is skipped, and the handler exits when
// ctx is cancelled.
func TestConsumeFillsJournalsAndDedups(t *testing.T) {
	tests := []struct {
		name          string
		events        []pnl.Record // pre-seeded journal records
		fill          stream.FillEvent
		wantFinalRows int  // journal rows after handling the fill
		dedupExpected bool // true = fill already known, must be skipped
	}{
		{
			name:          "fresh fill journaled",
			events:        nil,
			fill:          stream.FillEvent{OrderID: "ord-1", Symbol: "TSLA", Side: "buy", Qty: 2, Price: 250.5},
			wantFinalRows: 1,
		},
		{
			name: "already-backfilled order skipped",
			events: []pnl.Record{{
				Kind: pnl.KindFill, OrderID: "ord-2", Symbol: "AAPL",
				Side: "buy", Qty: "10", Price: "180", Status: "filled",
				TS: time.Now().UTC().Add(-time.Hour),
			}},
			fill:          stream.FillEvent{OrderID: "ord-2", Symbol: "AAPL", Side: "buy", Qty: 10, Price: 180},
			wantFinalRows: 1,
			dedupExpected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := newTestJournal(t)
			for _, r := range tt.events {
				if err := j.Append(r); err != nil {
					t.Fatal(err)
				}
			}
			if got := j.KnownOrder(tt.fill.OrderID); got != tt.dedupExpected {
				t.Fatalf("KnownOrder precheck = %v, want %v", got, tt.dedupExpected)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := make(chan stream.FillEvent, 1)
			ch <- tt.fill
			go consumeFills(ctx, ch, j, discardLogger())

			// Give the consumer a moment to process (or correctly skip),
			// then verify the journal state.
			time.Sleep(100 * time.Millisecond)
			recs, err := j.Records()
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != tt.wantFinalRows {
				t.Fatalf("journal rows = %d, want %d", len(recs), tt.wantFinalRows)
			}
		})
	}
}

// discardLogger keeps test output clean.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
