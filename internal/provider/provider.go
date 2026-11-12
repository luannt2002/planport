// Package provider defines the Provider interface that each backend type
// (codex, openai_compat, claude_cli) implements, and the OpenAI request types
// they share. A provider is one account; the server calls exactly the one the
// client names, never picking or switching.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Provider is one account of one backend.
type Provider interface {
	// ID is the name in PLANPORT_PROVIDERS, i.e. the "provider" field clients send.
	ID() string
	// StreamChat translates the request for the backend, calls upstream and
	// emits each StreamEvent.
	StreamChat(ctx context.Context, req ChatRequest, emit func(StreamEvent) error) error
}

// ModelLister is an optional capability; without it GET /v1/models returns 501.
type ModelLister interface {
	Models(context.Context) ([]Model, error)
}

// StreamEvent is one backend-neutral upstream event: a text chunk, usage, or
// Finish, an OpenAI finish_reason ("stop", "length", "content_filter"…) set
// only on the final event. StreamSSE uses Finish to tell done from cut.
type StreamEvent struct {
	Text   string
	Usage  *Usage
	Finish string
}

// Registry holds the providers in PLANPORT_PROVIDERS order.
type Registry []Provider

// ByID finds a provider by name, case-insensitively; the error lists the
// available names.
func (r Registry) ByID(id string) (Provider, error) {
	names := make([]string, len(r))
	for i, p := range r {
		if strings.EqualFold(p.ID(), id) {
			return p, nil
		}
		names[i] = p.ID()
	}
	return nil, fmt.Errorf("unknown provider %q (available: %s)", id, strings.Join(names, ", "))
}

// Model is one GET /v1/models entry (OpenAI format); ID is what clients send as
// "model". Created (0 if absent), DisplayName and Description come from
// upstream; the server sets Object and OwnedBy.
type Model struct {
	ID          string `json:"id"`
	Object      string `json:"object,omitempty"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
}

// ChatRequest is the POST /v1/chat/completions body; unknown fields are
// ignored. Provider names a provider in PLANPORT_PROVIDERS; Model is passed
// upstream as is; ReasoningEffort levels depend on the model, so the backend
// checks them (empty = model default).
type ChatRequest struct {
	Model           string        `json:"model"`
	Provider        string        `json:"provider,omitempty"`
	Messages        []ChatMessage `json:"messages"`
	Stream          bool          `json:"stream"`
	MaxTokens       *int          `json:"max_tokens,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

// ChatMessage is one OpenAI chat message. Content is a string or an array of
// parts; read it with Parts, or ContentText for text-only roles.
type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
}

// Part types.
const (
	PartText  = "text"
	PartImage = "image"
	PartFile  = "file"
)

// Part is one backend-neutral content part.
type Part struct {
	Type     string // PartText | PartImage | PartFile
	Text     string // PartText
	URL      string // PartImage: data URI or http(s) URL; PartFile: data URI (file_data)
	Filename string // PartFile
}

// Parts reads a string or an OpenAI part array, keeping order. Unsupported or
// incomplete parts are errors (400), never dropped: a dropped image still gets
// a 200 answer as if it was never sent.
func (m ChatMessage) Parts() ([]Part, error) {
	if len(m.Content) == 0 {
		return nil, nil
	}
	var s string
	// An array is never a string: skip that attempt, it would scan the whole (maybe 60 MB) content twice.
	if !bytes.HasPrefix(bytes.TrimLeft(m.Content, " \t\r\n"), []byte("[")) && json.Unmarshal(m.Content, &s) == nil {
		return []Part{{Type: PartText, Text: s}}, nil
	}
	var raw []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
		File struct {
			FileData string `json:"file_data"`
			Filename string `json:"filename"`
		} `json:"file"`
	}
	if err := json.Unmarshal(m.Content, &raw); err != nil {
		return nil, fmt.Errorf("content is neither a string nor an array of parts: %w", err)
	}
	parts := make([]Part, 0, len(raw))
	for i, p := range raw {
		switch p.Type {
		case "":
			// Untyped text is accepted (older clients); an untyped image/file/audio would vanish as text.
			if p.Text == "" || p.ImageURL.URL != "" || p.File.FileData != "" {
				return nil, fmt.Errorf("content[%d]: part missing \"type\" (image: \"image_url\", file: \"file\")", i)
			}
			fallthrough
		case "text":
			parts = append(parts, Part{Type: PartText, Text: p.Text})
		case "image_url":
			if p.ImageURL.URL == "" {
				return nil, fmt.Errorf("content[%d]: image_url.url is empty", i)
			}
			parts = append(parts, Part{Type: PartImage, URL: p.ImageURL.URL})
		case "file":
			if p.File.FileData == "" {
				return nil, fmt.Errorf("content[%d]: file needs file_data (base64 data URI); file_id is not supported", i)
			}
			parts = append(parts, Part{Type: PartFile, URL: p.File.FileData, Filename: p.File.Filename})
		default:
			return nil, fmt.Errorf("content[%d]: unsupported part %q (only text, image_url, file)", i, p.Type)
		}
	}
	return parts, nil
}

// ContentText returns the text of a text-only message (system, assistant);
// an image or file is an error.
func (m ChatMessage) ContentText() (string, error) {
	parts, err := m.Parts()
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, p := range parts {
		if p.Type != PartText {
			return "", fmt.Errorf("role %q accepts only text, got %s", m.Role, p.Type)
		}
		out.WriteString(p.Text)
	}
	return out.String(), nil
}

// Usage is token usage in OpenAI format.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Add returns u plus other.
func (u Usage) Add(other Usage) Usage {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	return u
}
