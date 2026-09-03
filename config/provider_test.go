package config

import (
	"os"
	"path/filepath"
	"testing"
)

// env vars via a temp .env file and asserts which backend Load picks.
type providerCase struct {
	name    string
	envFile string // extra KEY=VALUE lines beyond Alpaca keys
	env     map[string]string
	want    LLMProvider
	wantErr bool   // invalid LLM_PROVIDER value must fail Load
	wantURL string // expected OxloBaseURL ("" = skip)
}

func TestLLMProviderResolution(t *testing.T) {
	cases := []providerCase{
		{
			name:    "derived_llamacpp_when_base_url_set",
			envFile: "LLM_BASE_URL=http://127.0.0.1:8080\n",
			want:    ProviderLLamaCPP,
		},
		{
			name:    "derived_gemini_when_gemini_key_only",
			envFile: "GEMINI_API_KEY=gkey\n",
			want:    ProviderGemini,
		},
		{
			name:    "explicit_oxlo_overrides_base_url",
			envFile: "LLM_BASE_URL=http://127.0.0.1:8080\nOXLO_API_KEY=oxkey\n",
			env:     map[string]string{"LLM_PROVIDER": "oxlo"},
			want:    ProviderOxlo,
			wantURL: "https://api.oxlo.ai/v1",
		},
		{
			name:    "explicit_llamacpp_beats_derived_gemini",
			envFile: "GEMINI_API_KEY=gkey\n",
			env:     map[string]string{"LLM_PROVIDER": "llamacpp"},
			want:    ProviderLLamaCPP,
		},
		{
			name:    "explicit_gemini_overrides_base_url",
			envFile: "LLM_BASE_URL=http://127.0.0.1:8080\nGEMINI_API_KEY=gkey\n",
			env:     map[string]string{"LLM_PROVIDER": "gemini"},
			want:    ProviderGemini,
		},
		{
			name:    "case_insensitive_provider",
			envFile: "OXLO_API_KEY=oxkey\n",
			env:     map[string]string{"LLM_PROVIDER": " OXLO "},
			want:    ProviderOxlo,
		},
		{
			name:    "invalid_provider_rejected",
			envFile: "",
			env:     map[string]string{"LLM_PROVIDER": "anthropic"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Isolate from any ambient env first: loadEnvFile only sets a
			// var when it is not already present, so an empty t.Setenv
			// would block the .env line from applying.
			unsetForFile(t, "GEMINI_API_KEY", "LLM_BASE_URL", "OXLO_API_KEY", "FACTOR_MIN_SCORE", "FACTOR_WEIGHTS", "LLM_PROVIDER", "LLM_MODEL")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			p := filepath.Join(t.TempDir(), ".env")
			content := "ALPACA_API_KEY_ID=k\nALPACA_SECRET_KEY=s\n" + tc.envFile
			writeEnvFile(t, p, content)
			c, err := Load(p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got config %+v", c)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.LLMProvider != tc.want {
				t.Fatalf("provider = %q, want %q", c.LLMProvider, tc.want)
			}
			if tc.wantURL != "" && c.OxloBaseURL != tc.wantURL {
				t.Fatalf("OxloBaseURL = %q, want %q", c.OxloBaseURL, tc.wantURL)
			}
			if tc.env["LLM_PROVIDER"] == "oxlo" && c.OxloAPIKey != "oxkey" {
				t.Fatalf("OxloAPIKey not loaded, got %q", c.OxloAPIKey)
			}
		})
	}
}

func TestOxloKeyFromEnvFile(t *testing.T) {
	unsetForFile(t, "GEMINI_API_KEY", "LLM_BASE_URL", "OXLO_API_KEY", "FACTOR_MIN_SCORE", "FACTOR_WEIGHTS", "LLM_PROVIDER", "LLM_MODEL")
	p := filepath.Join(t.TempDir(), ".env")
	writeEnvFile(t, p, "ALPACA_API_KEY_ID=k\nALPACA_SECRET_KEY=s\nOXLO_API_KEY=sk_test123\nLLM_PROVIDER=oxlo\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.OxloAPIKey != "sk_test123" {
		t.Fatalf("OxloAPIKey = %q", c.OxloAPIKey)
	}
	if c.LLMModel != DefaultOxloModel {
		t.Fatalf("default oxlo model = %q, want %q", c.LLMModel, DefaultOxloModel)
	}
}

// unsetForFile removes vars so loadEnvFile will apply the .env line,
// restoring each on test cleanup.
func unsetForFile(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		old, had := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			}
		})
	}
}

func writeEnvFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
