// Package codex is the provider type for the ChatGPT Codex backend: Responses
// API with OAuth tokens from the Codex CLI auth file. Endpoints and headers
// come from env.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/luannt2002/planport/internal/provider"
	"github.com/luannt2002/planport/internal/tokenstore"
)

// Config is one codex provider; config.go fills each field from env.
type Config struct {
	Name           string        // provider ID (the "provider" body field)
	CredFile       string        // Codex CLI auth file
	BaseURL        string        // backend root; /responses is appended
	TokenURL       string        // OAuth refresh endpoint
	ClientID       string        // Codex CLI public client ID
	Originator     string        // originator header
	Beta           string        // OpenAI-Beta header
	UserAgent      string        // User-Agent header
	Timeout        time.Duration // cap on one upstream call, stream reading included
	RefreshMargin  time.Duration // refresh when the token expires sooner than this
	RefreshTimeout time.Duration // cap on one refresh
}

// Provider implements provider.Provider for one Codex account.
type Provider struct {
	cfg  Config
	cred *tokenstore.Store
}

// New returns a codex provider for cfg.
func New(cfg Config) *Provider {
	c := tokenstore.New(cfg.CredFile, "tokens", cfg.RefreshMargin, cfg.RefreshTimeout)
	c.AccessKey = "access_token"
	c.RefreshKey = "refresh_token"
	c.AccountKey = "account_id"
	c.LastRefreshKey = "last_refresh"
	c.RefreshURL = cfg.TokenURL
	c.ClientID = cfg.ClientID
	return &Provider{cfg: cfg, cred: c}
}

func (p *Provider) ID() string { return p.cfg.Name }

func toItem(role string, content []map[string]any) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": content}
}

// toContent maps user parts to Responses API content parts.
func toContent(parts []provider.Part) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case provider.PartImage:
			out = append(out, map[string]any{"type": "input_image", "image_url": p.URL})
		case provider.PartFile:
			out = append(out, map[string]any{"type": "input_file", "filename": p.Filename, "file_data": p.URL})
		default:
			out = append(out, map[string]any{"type": "input_text", "text": p.Text})
		}
	}
	return out
}

// toPayload translates an OpenAI request into a Responses API body. System
// messages join "instructions"; reasoning_effort becomes "reasoning": {"effort": …};
// max_tokens is dropped because the backend rejects max_output_tokens.
func toPayload(req provider.ChatRequest) (map[string]any, error) {
	var instructions []string
	var input []map[string]any
	for i, m := range req.Messages {
		var err error
		switch m.Role {
		case "system", "developer":
			var text string
			if text, err = m.ContentText(); err == nil {
				instructions = append(instructions, text)
			}
		case "assistant":
			var text string
			if text, err = m.ContentText(); err == nil {
				input = append(input, toItem(m.Role, []map[string]any{{"type": "output_text", "text": text}}))
			}
		case "user":
			var parts []provider.Part
			if parts, err = m.Parts(); err == nil {
				input = append(input, toItem(m.Role, toContent(parts)))
			}
		default:
			err = fmt.Errorf("role %q is not supported by the Codex backend", m.Role)
		}
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	payload := map[string]any{
		"model":        req.Model,
		"input":        input,
		"instructions": strings.Join(instructions, "\n\n"),
		"stream":       true, // the backend requires stream:true and store:false
		"store":        false,
	}
	if req.ReasoningEffort != "" {
		payload["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	return payload, nil
}

// decodeEvent maps one Responses API SSE event to a StreamEvent; only text
// deltas, usage and completion matter.
func decodeEvent(data []byte) (provider.StreamEvent, error) {
	var ev struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Response struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
		Error *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return provider.StreamEvent{}, fmt.Errorf("parse codex event: %w", err)
	}
	var usage *provider.Usage
	if u := ev.Response.Usage; u != nil {
		usage = &provider.Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.TotalTokens}
	}
	// A truncated answer (response.incomplete) must not report "stop".
	switch {
	case ev.Type == "response.output_text.delta":
		return provider.StreamEvent{Text: ev.Delta}, nil
	case ev.Type == "response.completed":
		return provider.StreamEvent{Finish: "stop", Usage: usage}, nil
	case ev.Type == "response.incomplete" && ev.Response.IncompleteDetails.Reason == "content_filter":
		return provider.StreamEvent{Finish: "content_filter", Usage: usage}, nil
	case ev.Type == "response.incomplete": // e.g. max_output_tokens
		return provider.StreamEvent{Finish: "length", Usage: usage}, nil
	case ev.Type == "response.failed" || strings.HasPrefix(ev.Type, "error"):
		err := fmt.Errorf("codex backend error: %s", provider.Truncate(data, provider.ErrSnippet))
		if ev.Error != nil && ev.Error.Type == "invalid_request_error" {
			// a bad request reported mid-stream (e.g. image URL not downloadable): 400
			return provider.StreamEvent{}, &provider.RequestError{Err: err}
		}
		return provider.StreamEvent{}, err
	}
	return provider.StreamEvent{}, nil // other events (created, in_progress…) are ignored
}

func (p *Provider) StreamChat(ctx context.Context, req provider.ChatRequest, emit func(provider.StreamEvent) error) error {
	payload, err := toPayload(req) // validate first: a bad request must not trigger a token refresh
	if err != nil {
		return &provider.RequestError{Err: err}
	}
	cred, err := p.cred.Get(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// Cap the whole call (stream included) with a context, not Client.Timeout,
	// so the error matches context.DeadlineExceeded and the server reports a timeout.
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return err
	}
	h := httpReq.Header
	h.Set("Authorization", "Bearer "+cred.AccessToken)
	if cred.AccountID != "" {
		h.Set("chatgpt-account-id", cred.AccountID)
	}
	h.Set("originator", p.cfg.Originator)
	h.Set("OpenAI-Beta", p.cfg.Beta)
	h.Set("User-Agent", p.cfg.UserAgent)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	return provider.StreamSSE(http.DefaultClient, httpReq, "codex /responses", decodeEvent, emit)
}
