package main

import (
	"strings"
	"testing"
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
