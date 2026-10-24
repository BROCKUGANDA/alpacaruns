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

// TestAutoTickHonorsPauseFlag asserts that a paused tick does NOT
// short-circuit the whole loop. Pause blocks NEW ENTRIES; the monitor
// pass (drawdown halts, TP/SL exits) must keep running so open
// positions stay protected while the operator has the bot paused.
//
// The loop is built by hand with no broker client: reaching the client
// call is the proof that the pause check did not abort the tick, and
// the tick reports an error instead of panicking.
func TestAutoTickHonorsPauseFlag(t *testing.T) {
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "paused")
	if err := os.WriteFile(flagPath, []byte("true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	loop := &autoLoop{
		pauseFlagPath: flagPath,
	}

	err := loop.tick(context.Background())
	if err == nil {
		t.Fatal("a paused tick must still run the monitor path (it reaches the broker), not return early")
	}
	if !strings.Contains(err.Error(), "broker client") {
		t.Fatalf("tick error = %v, want the broker-client error proving the monitor path ran", err)
	}
	if !loop.pausedLogged {
		t.Fatalf("pausedLogged should be true after first tick")
	}
	// Second tick still behaves the same.
	if err := loop.tick(context.Background()); err == nil {
		t.Fatal("second paused tick must still reach the monitor path")
	}
}

// TestAutoTickResumesWhenFlagCleared asserts that clearing the flag
// re-enables the entry pipeline. The pausedLogged flag flips back to
// false once the tick sees the cleared file.
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
	_ = loop.tick(context.Background())
	if loop.pausedLogged {
		t.Fatalf("pausedLogged should be false after resume")
	}
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
