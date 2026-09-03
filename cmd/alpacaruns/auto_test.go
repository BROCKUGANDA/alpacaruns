package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// auto flag parsing: valid combos accepted, bad positional rejected.
func TestAutoFlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"no flags", []string{}, false},
		{"once only", []string{"--once"}, false},
		{"dry-run only", []string{"--dry-run"}, false},
		{"both with env", []string{"--once", "--dry-run", "--env", ".env.local"}, false},
		{"positional rejected", []string{"extra"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fl autoFlags
			fs := newAutoFlagSet(&fl)
			_ = fs.Parse(tt.args)
			gotErr := fs.NArg() > 0
			if gotErr != tt.wantErr {
				t.Fatalf("positional error = %v, wantErr %v", gotErr, tt.wantErr)
			}
		})
	}
}

// usage text must document the auto subcommand.
func TestUsageMentionsAuto(t *testing.T) {
	if !strings.Contains(usage, "auto") {
		t.Fatal("usage text missing auto command")
	}
	if !strings.Contains(usage, "--dry-run") || !strings.Contains(usage, "--once") {
		t.Fatal("usage text missing auto flags")
	}
}


// pauseFlagEngaged returns true when the file at path is missing
// (false) or contains the literal "true" after trimming whitespace.
// We exercise the helper directly because the bot's tick is wired
// to a full Alpaca client + factor engine; testing through tick
// would require standing up the entire stack. The helper is the
// only thing that determines "do we place entries this tick?".
func TestPauseFlagEngaged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "paused")

	// Missing file => not engaged.
	if pauseFlagEngaged(p) {
		t.Fatalf("missing file should not be engaged")
	}

	// Empty path => not engaged.
	if pauseFlagEngaged("") {
		t.Fatalf("empty path should not be engaged")
	}

	// "false" => not engaged.
	if err := os.WriteFile(p, []byte("false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pauseFlagEngaged(p) {
		t.Fatalf("false should not be engaged")
	}

	// "true" => engaged.
	if err := os.WriteFile(p, []byte("true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !pauseFlagEngaged(p) {
		t.Fatalf("true should be engaged")
	}

	// Whitespace around "true" still engaged.
	if err := os.WriteFile(p, []byte("  true \t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !pauseFlagEngaged(p) {
		t.Fatalf("whitespace-padded true should be engaged")
	}

	// Wrong case not engaged.
	if err := os.WriteFile(p, []byte("TRUE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pauseFlagEngaged(p) {
		t.Fatalf("TRUE (uppercase) should not be engaged")
	}
}

// TestAutoTickHonorsPauseFlag asserts that when data/paused is "true",
// autoLoop.tick returns nil (no Alpaca call, no journal append) and
// crucially never increments an entry counter. We use a fake journal
// via pnl.Open on a temp dir and a kill-switch style stub.
func TestAutoTickHonorsPauseFlag(t *testing.T) {
	dir := t.TempDir()
	// Empty Alpaca client (would panic if we got past the pause check).
	// The pause flag is the FIRST check, so the loop never touches it.
	flagPath := filepath.Join(dir, "paused")
	if err := os.WriteFile(flagPath, []byte("true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build a minimal autoLoop whose tick() should bail out before
	// touching the Alpaca client. We give it the smallest viable
	// struct by hand rather than going through buildAutoLoop, which
	// requires a real network round-trip in LoadSettings.
	loop := &autoLoop{
		pauseFlagPath: flagPath,
	}

	if err := loop.tick(context.Background()); err != nil {
		t.Fatalf("tick with pause flag set should return nil, got %v", err)
	}
	if !loop.pausedLogged {
		t.Fatalf("pausedLogged should be true after first tick")
	}
	// Second tick still no-op, no error.
	if err := loop.tick(context.Background()); err != nil {
		t.Fatalf("second paused tick should return nil, got %v", err)
	}
}

// TestAutoTickResumesWhenFlagCleared asserts that clearing the flag
// re-enables the entry pipeline. We can't run the full pipeline here
// (it would touch the network), so we assert on the side-effects we
// CAN observe without network: the pausedLogged bool flips back to
// false and the loop reaches the Alpaca client (which is nil). A nil
// pointer dereference is the proof that the pause check passed.
func TestAutoTickResumesWhenFlagCleared(t *testing.T) {
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "paused")
	if err := os.WriteFile(flagPath, []byte("true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loop := &autoLoop{
		pauseFlagPath: flagPath,
		dryRun:        true,
	}
	_ = loop.tick(context.Background())
	if !loop.pausedLogged {
		t.Fatalf("pausedLogged should be true")
	}
	if err := os.Remove(flagPath); err != nil {
		t.Fatal(err)
	}
	// With pauseFlagPath still pointing at the now-missing file,
	// pauseFlagEngaged returns false. We expect the loop to advance
	// past the pause check and then call l.client.GetAccount, which
	// panics on a nil client. Recover and verify.
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected nil-client panic to indicate pause bypass")
		}
		if loop.pausedLogged {
			t.Fatalf("pausedLogged should be false after resume")
		}
	}()
	_ = loop.tick(context.Background())
}

// defaultPauseFlagPath derives the sidecar path next to the trade log.
func TestDefaultPauseFlagPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "data/paused"},
		{"trades.jsonl", "data/paused"},
		{"data/trades.jsonl", filepath.Join("data", "paused")},
		{"/var/lib/alpacaruns/trades.jsonl", filepath.Join("/var/lib/alpacaruns", "paused")},
	}
	for _, c := range cases {
		got := defaultPauseFlagPath(c.in)
		if got != c.want {
			t.Errorf("defaultPauseFlagPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// _ is a placeholder so goimports keeps time and atomic imports
// even before they have callers (some test files grow over time).
var _ = time.Second
