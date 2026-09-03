package main

import (
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/config"
)

func TestApplyProviderOverride(t *testing.T) {
	cases := []struct {
		name    string
		current config.LLMProvider
		flag    string
		want    config.LLMProvider
		wantErr bool
	}{
		{"empty_flag_noop", config.ProviderLLamaCPP, "", config.ProviderLLamaCPP, false},
		{"empty_flag_keeps_unset", "", "", "", false},
		{"override_oxlo", config.ProviderLLamaCPP, "oxlo", config.ProviderOxlo, false},
		{"override_llamacpp", config.ProviderGemini, "llamacpp", config.ProviderLLamaCPP, false},
		{"override_gemini", "", "gemini", config.ProviderGemini, false},
		{"same_value_ok", config.ProviderOxlo, "oxlo", config.ProviderOxlo, false},
		{"case_insensitive", config.ProviderLLamaCPP, "OXLO", config.ProviderOxlo, false},
		{"invalid_rejected", config.ProviderLLamaCPP, "chatgpt", config.ProviderLLamaCPP, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{LLMProvider: tc.current}
			err := applyProviderOverride(cfg, tc.flag)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error for %q", tc.flag)
				}
				for _, p := range []config.LLMProvider{config.ProviderLLamaCPP, config.ProviderOxlo, config.ProviderGemini} {
					if !contains(string(p), err.Error()) {
						t.Fatalf("error %q does not name allowed value %q", err, p)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LLMProvider != tc.want {
				t.Fatalf("provider = %q, want %q", cfg.LLMProvider, tc.want)
			}
		})
	}
}

func contains(sub, s string) bool {
	return len(sub) <= len(s) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
