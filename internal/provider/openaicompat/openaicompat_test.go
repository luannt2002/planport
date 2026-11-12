package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luannt2002/planport/internal/provider"
)

// seen is what the fake upstream received last.
type seen struct {
	path, auth string
	body       map[string]any
}

// upstream is a fake upstream replying status, Retry-After (if set) and body.
func upstream(status int, retryAfter, body string) (*httptest.Server, *seen) {
	got := &seen{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})), got
}

func newProvider(t *testing.T, base, key string) *Provider {
	t.Helper()
	cred := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(cred, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return New(Config{Name: "gm", CredFile: cred, BaseURL: base, Timeout: 5 * time.Second})
}

func hi() provider.ChatRequest {
	return provider.ChatRequest{Model: "up-model", Messages: []provider.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
}

// collect runs StreamChat and gathers the emitted text, finish and usage.
func collect(p *Provider, req provider.ChatRequest) (text, finish string, usage provider.Usage, err error) {
	err = p.StreamChat(context.Background(), req, func(ev provider.StreamEvent) error {
		text += ev.Text
		if ev.Finish != "" {
			finish = ev.Finish
		}
		if ev.Usage != nil {
			usage = usage.Add(*ev.Usage)
		}
		return nil
	})
	return
}

func TestStreamChatForwardsAndEmits(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"wörld — 世界\"},\"finish_reason\":\"length\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	srv, got := upstream(http.StatusOK, "", sse)
	defer srv.Close()
	n := 50
	req := provider.ChatRequest{Model: "up-model", MaxTokens: &n, ReasoningEffort: "low", Messages: []provider.ChatMessage{
		{Role: "system", Content: json.RawMessage(`"You are an assistant."`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"This image?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]`)},
	}}
	text, finish, usage, err := collect(newProvider(t, srv.URL, "test-key\n"), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/chat/completions" || got.auth != "Bearer test-key" {
		t.Errorf("path %q, Authorization %q: want /chat/completions and the key from the file (newline trimmed)", got.path, got.auth)
	}
	b := got.body
	if b["model"] != "up-model" || b["stream"] != true || b["max_tokens"] != float64(50) || b["reasoning_effort"] != "low" {
		t.Errorf("wrong upstream body (model must be the client's, passed through): model=%v stream=%v max_tokens=%v reasoning_effort=%v", b["model"], b["stream"], b["max_tokens"], b["reasoning_effort"])
	}
	if so, _ := b["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("missing stream_options.include_usage: %v", b["stream_options"])
	}
	if msgs, _ := b["messages"].([]any); len(msgs) != 2 {
		t.Errorf("messages must pass through (2 messages, image part included), got %v", b["messages"])
	}
	if text != "Hello wörld — 世界" || finish != "length" || usage.TotalTokens != 5 {
		t.Errorf("emitted text=%q finish=%q usage=%+v", text, finish, usage)
	}
}

// Non-200 upstream (chat or GET /models) → *UpstreamError with the same status
// and a labelled message; the wait comes from Retry-After or, for a bare 429,
// the body's error.resets_in_seconds.
func TestUpstreamError(t *testing.T) {
	quota := `{"error":{"message":"quota"}}`
	for _, tc := range []struct {
		status           int
		retryAfter, body string
		wait             time.Duration
	}{
		{http.StatusTooManyRequests, "7", quota, 7 * time.Second},
		{http.StatusUnauthorized, "7", quota, 7 * time.Second},
		{http.StatusForbidden, "7", quota, 7 * time.Second},
		{http.StatusTooManyRequests, "", `{"error":{"type":"usage_limit_reached","resets_in_seconds":42}}`, 42 * time.Second},
	} {
		srv, _ := upstream(tc.status, tc.retryAfter, tc.body)
		p := newProvider(t, srv.URL, "test-key")
		_, _, _, chatErr := collect(p, hi())
		_, modelsErr := p.Models(context.Background())
		srv.Close()
		for label, err := range map[string]error{"gm /chat/completions": chatErr, "gm GET /models": modelsErr} {
			var up *provider.UpstreamError
			if !errors.As(err, &up) || up.Status != tc.status || up.RetryAfter != tc.wait || !strings.HasPrefix(up.Msg, fmt.Sprintf("%s HTTP %d: ", label, tc.status)) {
				t.Errorf("%s HTTP %d: want *UpstreamError with the same status, RetryAfter %v, got %v", label, tc.status, tc.wait, err)
			}
		}
	}
}

// Missing / empty / inner-whitespace key file → error before calling upstream,
// not a client fault, and the message does not leak the key.
func TestBadKeyFile(t *testing.T) {
	srv, got := upstream(http.StatusOK, "", `{}`)
	defer srv.Close()
	missing := New(Config{Name: "gm", CredFile: filepath.Join(t.TempDir(), "missing"), BaseURL: srv.URL, Timeout: time.Second})
	for name, p := range map[string]*Provider{
		"missing file":  missing,
		"empty":         newProvider(t, srv.URL, " \n"),
		"inner space":   newProvider(t, srv.URL, "secret-1 secret-2\n"),
		"inner newline": newProvider(t, srv.URL, "secret-1\nsecret-2\n"),
	} {
		_, _, _, chatErr := collect(p, hi())
		_, modelsErr := p.Models(context.Background())
		for _, err := range []error{chatErr, modelsErr} {
			if err == nil || provider.ClientFault(err) || strings.Contains(err.Error(), "secret-") {
				t.Fatalf("%s: want a key error (not a client fault, key not leaked), got %v", name, err)
			}
		}
	}
	if got.path != "" {
		t.Fatal("upstream called despite a bad key")
	}
}

func TestStreamChatUnknownPartIs400(t *testing.T) {
	p := newProvider(t, "http://127.0.0.1:1", "test-key")
	_, _, _, err := collect(p, provider.ChatRequest{Model: "up-model",
		Messages: []provider.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"audio","x":1}]`)}}})
	if !provider.ClientFault(err) {
		t.Fatalf("unknown part must be a client fault (400), got %v", err)
	}
}

// Some upstreams send CUMULATIVE usage in every chunk: summing double-counts, so keep only the last.
func TestCumulativeUsageKeepsLast(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n\n"
	srv, _ := upstream(http.StatusOK, "", sse)
	defer srv.Close()
	text, _, usage, err := collect(newProvider(t, srv.URL, "test-key"), hi())
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalTokens != 13 || usage.CompletionTokens != 3 || text != "ab" {
		t.Fatalf("usage must be the last one (13/3), got %+v; text %q", usage, text)
	}
}

// Error mid-stream (HTTP 200, text already sent): the call fails, received text
// and usage are still emitted; invalid_request_error → client fault (400); a
// malformed chunk → error with a snippet.
func TestMidStreamError(t *testing.T) {
	text1 := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\n"
	for name, tc := range map[string]struct {
		tail        string
		clientFault bool
		wantInErr   string
	}{
		"upstream error":        {tail: `data: {"error":{"message":"timed out","type":"server_error"}}`, wantInErr: "timed out"},
		"invalid_request_error": {tail: `data: {"error":{"message":"bad image","type":"invalid_request_error"}}`, clientFault: true, wantInErr: "bad image"},
		"malformed chunk":       {tail: `data: {"choices":[{garbage`, wantInErr: "garbage"},
	} {
		srv, _ := upstream(http.StatusOK, "", text1+tc.tail+"\n\n")
		text, _, usage, err := collect(newProvider(t, srv.URL, "test-key"), hi())
		srv.Close()
		if err == nil || provider.ClientFault(err) != tc.clientFault || !strings.Contains(err.Error(), tc.wantInErr) {
			t.Fatalf("%s: wrong error (want clientFault %v): %v", name, tc.clientFault, err)
		}
		if text != "a" || usage.TotalTokens != 11 {
			t.Fatalf("%s: received text / usage must still be emitted: text=%q usage=%+v", name, text, usage)
		}
	}
}

// Upstream closes the stream without finishing (no finish_reason, no [DONE]),
// or ignores stream:true and sends plain JSON with HTTP 200: the call must fail
// with a plain error (server 502 / finish_reason "error"), not an empty 200
// "stop". Any finish_reason or [DONE] means done.
func TestStreamEndsBeforeCompletion(t *testing.T) {
	a := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"
	for name, tc := range map[string]struct {
		body   string
		wantOK bool
	}{
		"plain JSON, not SSE": {body: `{"choices":[{"message":{"content":"hi"}}]}`},
		"cut after text":      {body: a},
		"only [DONE]":         {body: a + "data: [DONE]\n\n", wantOK: true},
		"finish stop":         {body: a + "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", wantOK: true},
	} {
		srv, _ := upstream(http.StatusOK, "", tc.body)
		text, finish, _, err := collect(newProvider(t, srv.URL, "test-key"), hi())
		srv.Close()
		if tc.wantOK {
			// "stop" is normal: no other finish may reach the client.
			if err != nil || text != "a" || (finish != "" && finish != "stop") {
				t.Errorf("%s: want success, got text=%q finish=%q err=%v", name, text, finish, err)
			}
			continue
		}
		var up *provider.UpstreamError
		if err == nil || provider.ClientFault(err) || errors.As(err, &up) || !strings.Contains(err.Error(), "stream ended before completion") {
			t.Errorf("%s: want plain error \"stream ended before completion\", got %T %v", name, err, err)
		}
	}
}

// Models: standard GET {BASE_URL}/models with the Bearer key, decoding the data
// field; the server sets object/owned_by later, the provider invents nothing.
func TestModels(t *testing.T) {
	srv, got := upstream(http.StatusOK, "", `{"object":"list","data":[{"id":"m1","display_name":"Model one"},{"id":"m2"}]}`)
	defer srv.Close()
	ms, err := newProvider(t, srv.URL, "test-key").Models(context.Background())
	if err != nil || got.path != "/models" || got.auth != "Bearer test-key" {
		t.Fatalf("path=%q auth=%q err=%v", got.path, got.auth, err)
	}
	if len(ms) != 2 || ms[0].ID != "m1" || ms[0].DisplayName != "Model one" || ms[1].ID != "m2" {
		t.Fatalf("models=%+v", ms)
	}
}

// PLANPORT_TIMEOUT expiring (chat awaiting headers, chat mid-stream, GET /models)
// → an error matching context.DeadlineExceeded, so the server reports a timeout.
func TestTimeoutIsDeadline(t *testing.T) {
	for _, name := range []string{"chat awaiting headers", "chat mid-stream", "models"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if name == "chat mid-stream" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
				w.(http.Flusher).Flush()
			}
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		}))
		cfg := newProvider(t, srv.URL, "k").cfg
		cfg.Timeout = 200 * time.Millisecond
		p := New(cfg)
		var err error
		if name == "models" {
			_, err = p.Models(context.Background())
		} else {
			err = p.StreamChat(context.Background(), hi(), func(provider.StreamEvent) error { return nil })
		}
		srv.Close()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: want error matching context.DeadlineExceeded, got %v", name, err)
		}
	}
}
