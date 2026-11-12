// Package openaicompat is the provider type for any upstream speaking OpenAI
// chat completions with a Bearer API key; env picks the vendor endpoint.
// Requests pass through as is (model included) and always stream with usage.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/luannt2002/planport/internal/provider"
)

const maxModelsBody = 4 << 20 // GET /models body read into memory (memory safety, not config)

// Config is one openai_compat provider; config.go fills each field from env.
type Config struct {
	Name     string        // provider ID (the "provider" body field)
	CredFile string        // file holding the API key (one line)
	BaseURL  string        // API root; /chat/completions is appended
	Timeout  time.Duration // cap on one upstream call, stream reading included
}

// Provider implements provider.Provider and provider.ModelLister for one
// openai_compat account.
type Provider struct{ cfg Config }

// New returns an openai_compat provider for cfg.
func New(cfg Config) *Provider { return &Provider{cfg: cfg} }

func (p *Provider) ID() string { return p.cfg.Name }

// key reads the API key from the credential file on every request, so a new
// key needs no restart. Errors never contain the key.
func (p *Provider) key() (string, error) {
	b, err := os.ReadFile(p.cfg.CredFile)
	if err != nil {
		return "", fmt.Errorf("%s: read key file: %w", p.cfg.Name, err)
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("%s: key file %s is empty", p.cfg.Name, p.cfg.CredFile)
	}
	if strings.ContainsAny(k, " \t\r\n") { // two pasted keys or a note would cause a confusing upstream 401
		return "", fmt.Errorf("%s: key file %s must be a single line without whitespace", p.cfg.Name, p.cfg.CredFile)
	}
	return k, nil
}

func (p *Provider) StreamChat(ctx context.Context, req provider.ChatRequest, emit func(provider.StreamEvent) error) error {
	for i, m := range req.Messages {
		if _, err := m.Parts(); err != nil { // unsupported part: 400
			return &provider.RequestError{Err: fmt.Errorf("messages[%d]: %w", i, err)}
		}
	}
	payload := map[string]any{
		"model":          req.Model,
		"messages":       req.Messages,
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
	}
	if req.MaxTokens != nil {
		payload["max_tokens"] = *req.MaxTokens
	}
	if req.ReasoningEffort != "" {
		payload["reasoning_effort"] = req.ReasoningEffort
	}
	key, err := p.key()
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return &provider.RequestError{Err: err}
	}
	// Cap the whole call with a context (as in codex) so the error matches context.DeadlineExceeded.
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// Some upstreams send cumulative usage in every chunk, OpenAI only in the last: keeping the
	// last value is right for both. Emit it once when the stream ends, even after a mid-stream error.
	var last *provider.Usage
	err = provider.StreamSSE(http.DefaultClient, httpReq, p.cfg.Name+" /chat/completions", decodeChunk, func(ev provider.StreamEvent) error {
		if ev.Usage != nil {
			last, ev.Usage = ev.Usage, nil
		}
		return emit(ev)
	})
	if last != nil {
		if e := emit(provider.StreamEvent{Usage: last}); err == nil {
			err = e
		}
	}
	return err
}

// Models lists upstream models via the standard GET {BASE_URL}/models and
// returns them as is (the server sets object/owned_by). Non-200 →
// *UpstreamError with the same status, nothing guessed or filtered.
func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	key, err := p.key()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: GET /models: %w", p.cfg.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, provider.NewUpstreamError(resp, p.cfg.Name+" GET /models")
	}
	var out struct {
		Data []provider.Model `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelsBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: decode model list: %w", p.cfg.Name, err)
	}
	return out.Data, nil
}

// decodeChunk maps one chat completions chunk to a StreamEvent. finish_reason
// passes through ("stop" too: it marks the stream done); an error chunk fails
// the call.
func decodeChunk(data []byte) (provider.StreamEvent, error) {
	var c struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *provider.Usage `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return provider.StreamEvent{}, fmt.Errorf("decode chunk (%w): %s", err, provider.Truncate(data, provider.ErrSnippet))
	}
	if c.Error != nil {
		err := fmt.Errorf("upstream error mid-stream: %s", c.Error.Message)
		if c.Error.Type == "invalid_request_error" {
			return provider.StreamEvent{}, &provider.RequestError{Err: err} // bad request: 400
		}
		return provider.StreamEvent{}, err
	}
	ev := provider.StreamEvent{Usage: c.Usage}
	if len(c.Choices) > 0 {
		ev.Text = c.Choices[0].Delta.Content
		ev.Finish = c.Choices[0].FinishReason
	}
	return ev, nil
}
