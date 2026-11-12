package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// sse runs StreamSSE against a fake upstream replying status, Retry-After and body.
func sse(status int, retryAfter, body string, decode func([]byte) (StreamEvent, error)) error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	return StreamSSE(srv.Client(), req, "t", decode, func(StreamEvent) error { return nil })
}

func TestStreamSSE(t *testing.T) {
	stream := strings.Join([]string{
		"event: a",
		`data: {"n":1}`,
		"",
		": ignored comment",
		`data: {"n":2}`,
		"",
		"data: [DONE]",
		`data: {"n":3}`, // nothing after DONE is read
	}, "\n")
	var got []int
	err := sse(200, "", stream, func(data []byte) (StreamEvent, error) {
		var v struct {
			N int `json:"n"`
		}
		err := json.Unmarshal(data, &v)
		got = append(got, v.N)
		return StreamEvent{}, err
	})
	if err != nil || !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("got %v err=%v, want [1 2] and no error", got, err)
	}
	// EOF with neither [DONE] nor a finish event: a cut stream, never success.
	noop := func([]byte) (StreamEvent, error) { return StreamEvent{}, nil }
	if err := sse(200, "", `data: {"n":1}`, noop); err == nil || !strings.Contains(err.Error(), "stream ended before completion") {
		t.Fatalf("EOF without [DONE] must fail, got %v", err)
	}
}

func TestContentText(t *testing.T) {
	m := ChatMessage{Content: json.RawMessage(`"plain"`)}
	if s, _ := m.ContentText(); s != "plain" {
		t.Fatalf("string content wrong: %q", s)
	}
	m = ChatMessage{Content: json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)}
	if s, _ := m.ContentText(); s != "ab" {
		t.Fatalf("part array content wrong: %q", s)
	}
	// A text-only role (system/assistant) with an image must fail, not drop the image.
	m = ChatMessage{Content: json.RawMessage(`[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"https://x/a.png"}}]`)}
	if _, err := m.ContentText(); err == nil {
		t.Fatal("ContentText with an image must fail")
	}
}

// One turn mixing several texts, images and a file keeps its order.
func TestContentParts(t *testing.T) {
	m := ChatMessage{Content: json.RawMessage(`[
		{"type":"text","text":"question 1"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},
		{"type":"image_url","image_url":{"url":"https://x/b.jpg"}},
		{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,BBB"}},
		{"type":"text","text":"question 2"}]`)}
	parts, err := m.Parts()
	if err != nil {
		t.Fatal(err)
	}
	want := []Part{
		{Type: PartText, Text: "question 1"},
		{Type: PartImage, URL: "data:image/png;base64,AAA"},
		{Type: PartImage, URL: "https://x/b.jpg"},
		{Type: PartFile, URL: "data:application/pdf;base64,BBB", Filename: "a.pdf"},
		{Type: PartText, Text: "question 2"},
	}
	if !slices.Equal(parts, want) {
		t.Fatalf("got %+v\nwant %+v", parts, want)
	}
	// A text-only part without "type" is still text (older clients send this).
	if parts, err := (ChatMessage{Content: json.RawMessage(`[{"text":"no type"}]`)}).Parts(); err != nil || len(parts) != 1 || parts[0] != (Part{Type: PartText, Text: "no type"}) {
		t.Fatalf("text part without type must be text: %+v %v", parts, err)
	}
	if parts, _ := (ChatMessage{Content: json.RawMessage(`"text only"`)}).Parts(); len(parts) != 1 || parts[0] != (Part{Type: PartText, Text: "text only"}) {
		t.Fatalf("string content must become 1 text part: %+v", parts)
	}
	// Unsupported or incomplete parts: a clear error, never silently dropped.
	for _, bad := range []string{
		`[{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}]`,
		`[{"type":"image_url","image_url":{}}]`,
		`[{"type":"file","file":{"file_id":"file-1"}}]`,
		// no "type" and not text (image, file, audio): must not become empty text
		`[{"image_url":{"url":"data:image/png;base64,AAA"}}]`,
		`[{"file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,BBB"}}]`,
		`[{"file":{"file_id":"file-1"}}]`,
		`[{"input_audio":{"data":"x","format":"wav"}}]`,
	} {
		if _, err := (ChatMessage{Content: json.RawMessage(bad)}).Parts(); err == nil {
			t.Fatalf("must fail: %s", bad)
		}
	}
}

// Over real HTTP, an upstream error becomes an UpstreamError with the right
// status and wait, which the server turns into the HTTP status + Retry-After.
// Codex sends the quota reset in the 429 body (error.resets_in_seconds).
func TestStreamSSEUpstreamError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header string
		body   string
		want   time.Duration
	}{
		{"429 + Retry-After", 429, "120", `{"error":{"message":"x"}}`, 2 * time.Minute},
		{"429 resets_in_seconds only", 429, "", `{"error":{"type":"usage_limit_reached","resets_at":1,"limit_window_minutes":300,"resets_in_seconds":4313}}`, 4313 * time.Second},
		{"503", 503, "", `{"error":{"resets_in_seconds":9}}`, 0},
	}
	for _, tc := range cases {
		err := sse(tc.status, tc.header, tc.body, nil)
		var up *UpstreamError
		if !errors.As(err, &up) || up.Status != tc.status || up.RetryAfter != tc.want {
			t.Fatalf("%s: want UpstreamError{%d, %v}, got %T %+v", tc.name, tc.status, tc.want, err, err)
		}
	}
}

// Truncate must not split a UTF-8 character: the JSON error sent to the client
// would show "�".
func TestTruncateUTF8(t *testing.T) {
	s := "error © — sign in again"
	for n := 0; n <= len(s); n++ {
		got := Truncate([]byte(s), n)
		if !utf8.ValidString(got) {
			t.Fatalf("n=%d split a character: %q", n, got)
		}
	}
	if got := Truncate([]byte("abc"), 5); got != "abc" {
		t.Fatalf("string shorter than n must stay as is: %q", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":                              0,
		"120":                           2 * time.Minute,
		"-5":                            0,
		"99999999999999999":             0, // overflows int64 as a Duration
		"garbage":                       0,
		"Fri, 23 Oct 2026 10:01:30 GMT": 90 * time.Second,
	} {
		if got := parseRetryAfter(v, now); got != want {
			t.Fatalf("parseRetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}

type fake string

func (f fake) ID() string                                                           { return string(f) }
func (fake) StreamChat(context.Context, ChatRequest, func(StreamEvent) error) error { return nil }

func TestByID(t *testing.T) {
	r := Registry{fake("codex"), fake("compat")}
	if p, err := r.ByID("compat"); err != nil || p.ID() != "compat" {
		t.Fatalf("ByID(compat) wrong: %v %v", p, err)
	}
	if p, err := r.ByID("Codex"); err != nil || p.ID() != "codex" {
		t.Fatalf("ByID must match case-insensitively: %v %v", p, err)
	}
	if _, err := r.ByID("missing"); err == nil || !strings.Contains(err.Error(), "codex, compat") {
		t.Fatalf("unknown provider must fail listing the names: %v", err)
	}
}
