// Package llamacpp implements ADK's model.LLM against llama.cpp's
// llama-server OpenAI-compatible endpoint (/v1/chat/completions),
// including function/tool calling.
//
// IMPORTANT (Qwen3 family): start llama-server WITH --jinja so the
// model's own chat-template handles tool-call serialization. Mismatched
// templates are the most common cause of malformed tool calls locally:
//
//	llama-server -m Qwen3-4B-Instruct-Q4_K_M.gguf --jinja -c 16384
package llamacpp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/model"
)

// Model talks to an OpenAI-compatible server exposing /v1/chat/completions
// (local llama.cpp llama-server, or a hosted provider such as Oxlo.ai).
type Model struct {
	baseURL string
	model   string
	apiKey  string // optional bearer token for hosted providers
	http    *http.Client
	log     *slog.Logger
}

func New(baseURL, modelName string, log *slog.Logger) *Model {
	return NewWithKey(baseURL, modelName, "", log)
}

// NewWithKey builds a Model that sends Authorization: Bearer <apiKey> on
// every request. Pass an empty apiKey to talk unauthenticated (local
// llama-server); New is the shorthand for that case.
func NewWithKey(baseURL, modelName, apiKey string, log *slog.Logger) *Model {
	if log == nil {
		log = slog.Default()
	}
	return &Model{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   modelName,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 5 * time.Minute},
		log:     log,
	}
}


func (m *Model) Name() string { return "llamacpp/" + m.model }

// ---- request conversion (genai -> OpenAI) ----

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string         `json:"type"`
	Function oaFunctionSpec `json:"function"`
}

type oaFunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type oaRequest struct {
	Model       string      `json:"model"`
	Messages    []oaMessage `json:"messages"`
	Tools       []oaTool    `json:"tools,omitempty"`
	Temperature float64     `json:"temperature,omitempty"`
	Stream      bool        `json:"stream"`
}

func convertMessages(contents []*genai.Content) ([]oaMessage, error) {
	var out []oaMessage
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			switch {
			case p.Text != "":
				out = append(out, oaMessage{Role: roleOf(c.Role), Content: p.Text})
			case p.FunctionCall != nil:
				args, err := json.Marshal(p.FunctionCall.Args)
				if err != nil {
					return nil, err
				}
				tc := oaToolCall{ID: p.FunctionCall.ID, Type: "function"}
				tc.Function.Name = p.FunctionCall.Name
				tc.Function.Arguments = string(args)
				out = append(out, oaMessage{Role: "assistant", ToolCalls: []oaToolCall{tc}})
			case p.FunctionResponse != nil:
				respBytes, err := json.Marshal(p.FunctionResponse.Response)
				if err != nil {
					return nil, err
				}
				out = append(out, oaMessage{
					Role:       "tool",
					ToolCallID: p.FunctionResponse.ID,
					Content:    string(respBytes),
				})
			}
		}
	}
	return out, nil
}

func roleOf(r string) string {
	if r == genai.RoleUser || r == "user" {
		return "user"
	}
	return "assistant"
}

func convertTools(req *model.LLMRequest) []oaTool {
	var tools []oaTool
	if req.Config == nil {
		return tools
	}
	for _, td := range req.Config.Tools {
		if td == nil {
			continue
		}
		for _, f := range td.FunctionDeclarations {
			params := map[string]any{"type": "object", "properties": map[string]any{}}
			if f.ParametersJsonSchema != nil {
				params = anyToMap(f.ParametersJsonSchema)
			} else if f.Parameters != nil {
				params = schemaToMap(f.Parameters)
			}
			tools = append(tools, oaTool{
				Type: "function",
				Function: oaFunctionSpec{
					Name:        f.Name,
					Description: f.Description,
					Parameters:  params,
				},
			})
		}
	}
	return tools
}

func anyToMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{"type": "object"}
	}
	return out
}

func schemaToMap(s *genai.Schema) map[string]any {
	m := map[string]any{}
	if s.Type != "" {
		m["type"] = strings.ToLower(string(s.Type))
	}
	if s.Description != "" {
		m["description"] = s.Description
	}
	if len(s.Properties) > 0 {
		props := map[string]any{}
		for k, v := range s.Properties {
			props[k] = schemaToMap(v)
		}
		m["properties"] = props
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if len(s.Enum) > 0 {
		m["enum"] = s.Enum
	}
	return m
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---- response conversion (OpenAI -> genai) ----

type oaResponse struct {
	Choices []struct {
		Message struct {
			Content   string       `json:"content"`
			ToolCalls []oaToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// GenerateContent implements model.LLM (non-streaming; stream=false always).
func (m *Model) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		msgs, err := convertMessages(req.Contents)
		if err != nil {
			yield(nil, fmt.Errorf("llamacpp: convert contents: %w", err))
			return
		}
		body := oaRequest{
			Model:    m.model,
			Messages: msgs,
			Tools:    convertTools(req),
		}
		if req.Config != nil && req.Config.Temperature != nil {
			body.Temperature = float64(*req.Config.Temperature)
		}
		start := time.Now()
		b, _ := json.Marshal(body)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			m.baseURL+"/v1/chat/completions", bytes.NewReader(b))
		if err != nil {
			yield(nil, err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if m.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+m.apiKey)
		}
		m.log.Info("llamacpp request", "model", m.model, "messages", len(msgs), "tools", len(body.Tools))
		resp, err := m.http.Do(httpReq)
		if err != nil {
			yield(nil, fmt.Errorf("llamacpp: %w", err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(resp.Body)
			yield(nil, fmt.Errorf("llamacpp: HTTP %d: %s", resp.StatusCode, buf.String()))
			return
		}
		var or oaResponse
		if err := json.NewDecoder(resp.Body).Decode(&or); err != nil {
			yield(nil, fmt.Errorf("llamacpp: decode: %w", err))
			return
		}
		if or.Error != nil {
			yield(nil, fmt.Errorf("llamacpp: %s", or.Error.Message))
			return
		}
		if len(or.Choices) == 0 {
			yield(nil, fmt.Errorf("llamacpp: empty choices"))
			return
		}
		m.log.Info("llamacpp response", "latency", time.Since(start).String(), "finish", or.Choices[0].FinishReason)
		msg := or.Choices[0].Message
		content := &genai.Content{Role: genai.RoleModel}
		if msg.Content != "" {
			content.Parts = append(content.Parts, &genai.Part{Text: msg.Content})
		}
		for _, tc := range msg.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			content.Parts = append(content.Parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: args,
				},
			})
		}
		lr := &model.LLMResponse{
			Content:      content,
			TurnComplete: true,
			ModelVersion: m.model,
		}
		switch or.Choices[0].FinishReason {
		case "tool_calls":
			lr.FinishReason = genai.FinishReasonStop
		default:
			lr.FinishReason = genai.FinishReasonStop
		}
		yield(lr, nil)
	}
}

var _ model.LLM = (*Model)(nil)
