package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/luannt2002/planport/internal/provider"
)

func msg(role, content string) provider.ChatMessage {
	return provider.ChatMessage{Role: role, Content: json.RawMessage(content)}
}

// sameJSON fails unless got and want marshal to the same JSON.
func sameJSON(t *testing.T, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("wrong payload:\n got  %s\n want %s", g, w)
	}
}

// system joins "instructions"; user → input_text, assistant → output_text with
// the text unchanged; always stream:true store:false; reasoning_effort becomes
// "reasoning": {"effort": …} and is absent when not sent.
func TestChatToCodexPayload(t *testing.T) {
	req := provider.ChatRequest{Model: "pa-model", Messages: []provider.ChatMessage{
		msg("system", `"You are an assistant."`), msg("user", `"Hello"`), msg("assistant", `"Hi there"`), msg("user", `"1+1=?"`),
	}}
	item := func(role, typ, text string) map[string]any {
		return map[string]any{"type": "message", "role": role, "content": []map[string]any{{"type": typ, "text": text}}}
	}
	want := map[string]any{
		"model": "pa-model", "instructions": "You are an assistant.", "stream": true, "store": false,
		"input": []map[string]any{item("user", "input_text", "Hello"), item("assistant", "output_text", "Hi there"), item("user", "input_text", "1+1=?")},
	}
	payload, err := toPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, payload, want)

	req.ReasoningEffort = "e-x" // neutral value: code must not keep a list of levels
	want["reasoning"] = map[string]any{"effort": "e-x"}
	payload, err = toPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, payload, want)
}

// OpenAI image/file parts become Responses API input_image/input_file, in order.
func TestCodexMultimodalPayload(t *testing.T) {
	req := provider.ChatRequest{Model: "pa-model", Messages: []provider.ChatMessage{msg("user", `[
		{"type":"text","text":"compare"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},
		{"type":"image_url","image_url":{"url":"https://x/b.jpg"}},
		{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,BBB"}}]`)}}
	payload, err := toPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, payload["input"].([]map[string]any)[0]["content"], []map[string]any{
		{"type": "input_text", "text": "compare"},
		{"type": "input_image", "image_url": "data:image/png;base64,AAA"},
		{"type": "input_image", "image_url": "https://x/b.jpg"},
		{"type": "input_file", "filename": "a.pdf", "file_data": "data:application/pdf;base64,BBB"},
	})
	// system is text-only: an image there must be a 400, not silently dropped.
	req.Messages[0].Role = "system"
	if _, err := toPayload(req); err == nil {
		t.Fatal("image in system must fail")
	}
}

func TestCodexEvent(t *testing.T) {
	usage := func(in, out, total int) *provider.Usage {
		return &provider.Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: total}
	}
	for _, tc := range []struct {
		data string
		want provider.StreamEvent
	}{
		{`{"type":"response.output_text.delta","delta":"café — 世界"}`, provider.StreamEvent{Text: "café — 世界"}},
		{`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`, provider.StreamEvent{Finish: "stop", Usage: usage(10, 5, 15)}},
		// Truncated (real payload) → "length", content filter → "content_filter":
		// otherwise the client takes the answer as complete.
		{`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":23,"output_tokens":16,"total_tokens":39}}}`, provider.StreamEvent{Finish: "length", Usage: usage(23, 16, 39)}},
		{`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}`, provider.StreamEvent{Finish: "content_filter"}},
		{`{"type":"response.created"}`, provider.StreamEvent{}}, // ignored, like in_progress…
	} {
		if got, err := decodeEvent([]byte(tc.data)); err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v err=%v, want %+v", tc.data, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		data        string
		clientFault bool
	}{
		{`{"type":"response.failed"}`, false}, // backend error → 502
		// A bad request reported mid-stream (real payload: the backend could not download the image URL) → 400.
		{`{"type":"error","error":{"message":"Error while downloading file. Upstream status code: 400.","type":"invalid_request_error","param":"url","code":"invalid_value"}}`, true},
		{`{"type":"error","error":{"type":"server_error","message":"x"}}`, false},
	} {
		_, err := decodeEvent([]byte(tc.data))
		var re *provider.RequestError
		if err == nil || errors.As(err, &re) != tc.clientFault {
			t.Errorf("%s: want an error with RequestError=%v, got %T %v", tc.data, tc.clientFault, err, err)
		}
	}
}

// A request that cannot be translated is rejected BEFORE credentials are touched.
func TestBadRequestCheckedBeforeCredentials(t *testing.T) {
	p := New(Config{CredFile: "/nonexistent/auth.json"})
	req := provider.ChatRequest{Model: "pa-model", Messages: []provider.ChatMessage{msg("tool", `"x"`)}}
	err := p.StreamChat(context.Background(), req, func(provider.StreamEvent) error { return nil })
	var re *provider.RequestError
	if !errors.As(err, &re) {
		t.Fatalf("want RequestError (request checked before credentials), got %T: %v", err, err)
	}
}

// newTestProvider returns a codex provider aimed at baseURL with a fake auth
// file: the JWT access token is valid for 1 hour (no refresh).
func newTestProvider(t *testing.T, baseURL, accountID string) (*Provider, string) {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	access := "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
	tokens := map[string]any{"access_token": access, "refresh_token": "r"}
	if accountID != "" {
		tokens["account_id"] = accountID
	}
	auth, _ := json.Marshal(map[string]any{"tokens": tokens})
	cred := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(cred, auth, 0o600); err != nil {
		t.Fatal(err)
	}
	return New(Config{Name: "cx", CredFile: cred, BaseURL: baseURL, Originator: "orig-x", Beta: "beta-x", UserAgent: "ua-x",
		Timeout: 5 * time.Second, RefreshMargin: time.Minute, RefreshTimeout: time.Second}), access
}

func hi() provider.ChatRequest {
	return provider.ChatRequest{Model: "pa-model", Messages: []provider.ChatMessage{msg("user", `"hi"`)}}
}

// The request to the Codex backend has the right path, token, account and
// identity headers from Config, and a stream:true store:false body.
func TestStreamChatHeaders(t *testing.T) {
	for _, account := range []string{"acc-1", ""} {
		var path string
		var hdr http.Header
		var body map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path, hdr = r.URL.Path, r.Header.Clone()
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
		}))
		p, access := newTestProvider(t, srv.URL, account)
		err := p.StreamChat(context.Background(), hi(), func(provider.StreamEvent) error { return nil })
		srv.Close()
		if err != nil {
			t.Fatalf("account %q: %v", account, err)
		}
		if path != "/responses" {
			t.Errorf("path %q, want /responses", path)
		}
		for k, want := range map[string]string{
			"Authorization":      "Bearer " + access,
			"chatgpt-account-id": account, // no account id, no header
			"originator":         "orig-x",
			"OpenAI-Beta":        "beta-x",
			"User-Agent":         "ua-x",
		} {
			if got := hdr.Get(k); got != want {
				t.Errorf("account %q: header %s = %q, want %q", account, k, got, want)
			}
		}
		if _, has := hdr["Chatgpt-Account-Id"]; account == "" && has {
			t.Errorf("chatgpt-account-id header sent without an account id")
		}
		if body["stream"] != true || body["store"] != false || body["model"] != "pa-model" {
			t.Errorf("body must be stream:true store:false with model passed through, got stream=%v store=%v model=%v", body["stream"], body["store"], body["model"])
		}
	}
}

// Upstream closes the body cleanly before response.completed: received text
// is still emitted, but the call must fail with a plain error (server 502 /
// finish_reason "error"), never report a partial answer as success.
func TestStreamCutBeforeCompleted(t *testing.T) {
	delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n"
	for name, tc := range map[string]struct {
		sse    string
		wantOK bool
	}{
		"cut after delta": {sse: delta},
		"empty body":      {sse: ""},
		"completed":       {sse: delta + "data: {\"type\":\"response.completed\",\"response\":{}}\n\n", wantOK: true},
		"incomplete":      {sse: delta + "data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", wantOK: true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.sse) }))
		p, _ := newTestProvider(t, srv.URL, "acc-1")
		var text string
		err := p.StreamChat(context.Background(), hi(), func(ev provider.StreamEvent) error {
			text += ev.Text
			return nil
		})
		srv.Close()
		if tc.wantOK {
			if err != nil || text != "Hi" {
				t.Errorf("%s: want success, got text=%q err=%v", name, text, err)
			}
			continue
		}
		var up *provider.UpstreamError
		if err == nil || provider.ClientFault(err) || errors.As(err, &up) || !strings.Contains(err.Error(), "stream ended before completion") {
			t.Errorf("%s: want plain error \"stream ended before completion\", got %T %v", name, err, err)
		}
	}
}

// PLANPORT_TIMEOUT expiring (awaiting headers or mid-stream) → an error matching
// context.DeadlineExceeded, so the server reports a timeout instead of a generic error.
func TestStreamTimeoutIsDeadline(t *testing.T) {
	for name, partial := range map[string]bool{"awaiting headers": false, "mid-stream": true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if partial {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n")
				w.(http.Flusher).Flush()
			}
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		}))
		p, _ := newTestProvider(t, srv.URL, "")
		cfg := p.cfg
		cfg.Timeout = 200 * time.Millisecond
		err := New(cfg).StreamChat(context.Background(), hi(), func(provider.StreamEvent) error { return nil })
		srv.Close()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: want error matching context.DeadlineExceeded, got %v", name, err)
		}
	}
}
