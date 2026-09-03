package llamacpp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestGenerateContentTextOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "qwen3-test" {
			t.Fatalf("model = %v", req["model"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": "RSI is 62.3", "role": "assistant"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	m := New(srv.URL, "qwen3-test", nil)
	resp, err := first(m.GenerateContent(context.Background(), &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "AAPL RSI?"}}}},
	}, false))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Content.Parts[0].Text; got != "RSI is 62.3" {
		t.Fatalf("text = %q", got)
	}
}

func TestGenerateContentToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []json.RawMessage `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) == 0 {
			t.Fatal("expected tools in request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "get_quotes",
							"arguments": `{"symbols":["AAPL"]}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
		})
	}))
	defer srv.Close()

	m := New(srv.URL, "qwen3-test", nil)
	resp, err := first(m.GenerateContent(context.Background(), &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "quote AAPL"}}}},
		Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{
			FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name:        "get_quotes",
				Description: "latest quotes",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"symbols": {Type: genai.TypeArray}}},
			}},
		}}},
	}, false))
	if err != nil {
		t.Fatal(err)
	}
	fc := resp.Content.Parts[0].FunctionCall
	if fc == nil || fc.Name != "get_quotes" {
		t.Fatalf("expected function call, got %+v", resp.Content.Parts[0])
	}
	syms, ok := fc.Args["symbols"].([]any)
	if !ok || len(syms) != 1 || syms[0] != "AAPL" {
		t.Fatalf("args = %+v", fc.Args)
	}
}

func TestFunctionResponseRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var toolMsg *string
		for i := range req.Messages {
			if req.Messages[i].Role == "tool" {
				s := req.Messages[i].Content
				toolMsg = &s
			}
		}
		if toolMsg == nil {
			t.Fatal("expected tool message in converted contents")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": "bid 101", "role": "assistant"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	m := New(srv.URL, "qwen3-test", nil)
	resp, err := first(m.GenerateContent(context.Background(), &model.LLMRequest{
		Contents: []*genai.Content{{
			Role: "user",
			Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				ID:       "call_1",
				Name:     "get_quotes",
				Response: map[string]any{"bp": 101.0},
			}}},
		}},
	}, false))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content.Parts[0].Text != "bid 101" {
		t.Fatalf("unexpected: %+v", resp.Content.Parts[0])
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model failed to load", http.StatusInternalServerError)
	}))
	defer srv.Close()
	m := New(srv.URL, "qwen3-test", nil)
	if _, err := first(m.GenerateContent(context.Background(), &model.LLMRequest{}, false)); err == nil {
		t.Fatal("expected error")
	}
}

// helper to drain the iterator
func first(seq func(yield func(*model.LLMResponse, error) bool)) (*model.LLMResponse, error) {
	var resp *model.LLMResponse
	var err error
	seq(func(r *model.LLMResponse, e error) bool {
		resp, err = r, e
		return false // stop after first
	})
	return resp, err
}

// TestAuthHeaderInjection asserts the bearer token is sent only when a key
// is configured (Oxlo.ai and other hosted OpenAI-compatible providers).
func TestAuthHeaderInjection(t *testing.T) {
	cases := []struct {
		name   string
		apiKey string
		want   string // expected Authorization header; "" = must be absent
	}{
		{"with_key", "sk-test-123", "Bearer sk-test-123"},
		{"without_key", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{
						"message": map[string]any{"role": "assistant", "content": "ok"},
					}},
				})
			}))
			defer srv.Close()

			m := NewWithKey(srv.URL, "test-model", tc.apiKey, nil)
			resp, err := first(m.GenerateContent(context.Background(), &model.LLMRequest{
				Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}},
			}, false))
			if err != nil {
				t.Fatal(err)
			}
			if resp == nil {
				t.Fatal("nil response")
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("Authorization = %q, want %q", got, tc.want)
			}
			if tc.want == "" && got != "" {
				t.Fatalf("Authorization unexpectedly set to %q for empty key", got)
			}
		})
	}
}
