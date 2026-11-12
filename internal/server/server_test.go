package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/luannt2002/planport/internal/provider"
)

// fake emits text once (an empty text is like an upstream response.created
// event) and returns err; with err set and late false it fails before emitting.
type fake struct {
	id     string
	text   string
	finish string // e.g. "length" when the backend truncated
	err    error
	late   bool
	calls  int
	got    provider.ChatRequest
}

func (f *fake) ID() string { return f.id }
func (f *fake) StreamChat(_ context.Context, req provider.ChatRequest, emit func(provider.StreamEvent) error) error {
	f.calls++
	f.got = req
	if f.err != nil && !f.late {
		return f.err
	}
	_ = emit(provider.StreamEvent{Text: f.text, Finish: f.finish})
	return f.err
}

type catalogProvider struct {
	fake
	models  []provider.Model
	listErr error
	lists   int
}

func (p *catalogProvider) Models(context.Context) ([]provider.Model, error) {
	p.lists++
	return p.models, p.listErr
}

func call(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body)))
	return rec
}

func chatBody(pid string, stream bool) string {
	return fmt.Sprintf(`{"provider":%q,"model":"m-x","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, pid, stream)
}

// captureLog sends the server log to the returned buffer for this test.
func captureLog(t *testing.T) *strings.Builder {
	var b strings.Builder
	old := log.Writer()
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(old) })
	return &b
}

// Bad input is a 4xx before any provider call: 400 (bad JSON, trailing data,
// missing fields), 404 listing the names, 413 past the cap. Trailing whitespace
// (curl -d @file) is fine.
func TestChatInput(t *testing.T) {
	defer func(old int64) { maxChatBody = old }(maxChatBody)
	maxChatBody = 1 << 10
	ok := chatBody("acc1", false)
	for name, tc := range map[string]struct {
		body string
		code int
		want string
	}{
		"empty body":          {``, 400, "invalid JSON body"},
		"broken JSON":         {`{"provider":"acc1","model":`, 400, "invalid JSON body"},
		"no provider":         {`{"model":"m-x","messages":[{"role":"user","content":"hi"}]}`, 400, "missing provider, model or messages"},
		"empty provider":      {`{"provider":"","model":"m-x","messages":[{"role":"user","content":"hi"}]}`, 400, "missing provider"},
		"empty model":         {`{"provider":"acc1","model":"","messages":[{"role":"user","content":"hi"}]}`, 400, "missing provider"},
		"empty messages":      {`{"provider":"acc1","model":"m-x","messages":[]}`, 400, "missing provider"},
		"null messages":       {`{"provider":"acc1","model":"m-x","messages":null}`, 400, "missing provider"},
		"unknown provider":    {chatBody("unknown", false), 404, "acc1, acc2"},
		"trailing junk":       {ok + "junk", 400, "invalid JSON body"},
		"second JSON value":   {ok + `{"provider":"acc1"}`, 400, "invalid JSON body"},
		"junk over cap":       {ok + strings.Repeat("x", 2<<10), 413, "too large"},
		"body over cap":       {`{"provider":"acc1","model":"m-x","messages":[{"role":"user","content":"` + strings.Repeat("a", 2<<10) + `"}]}`, 413, "too large"},
		"trailing whitespace": {ok + " \n", 200, "hello"},
	} {
		acc1 := &fake{id: "acc1", text: "hello"}
		rec := call(t, New(provider.Registry{acc1, &fake{id: "acc2"}}, ""), "POST", "/v1/chat/completions", tc.body)
		b := rec.Body.String()
		if rec.Code != tc.code || !strings.Contains(b, tc.want) || (tc.code != 200 && acc1.calls != 0) ||
			(tc.code == 400 || tc.code == 413) && !strings.Contains(b, `"type":"invalid_request_error"`) {
			t.Errorf("%s: want %d with %q, no call: %d calls=%d %s", name, tc.code, tc.want, rec.Code, acc1.calls, b)
		}
	}
}

// Provider errors keep a real HTTP status in both modes (no text was sent) and
// never switch to another provider. 502 hides internals (credential paths,
// URLs, 401/403 bodies that may echo the key) that the server log keeps; a
// timeout names PLANPORT_TIMEOUT; 400 and 429 pass the upstream message through.
func TestChatErrors(t *testing.T) {
	logBuf := captureLog(t)
	for name, tc := range map[string]struct {
		err     error
		late    bool // fail after an empty event, before any text
		code    int
		typ     string
		retry   string
		want    []string // in the message
		notWant []string // not in the body
		inLog   string
	}{
		"429 + Retry-After":       {err: &provider.UpstreamError{Status: 429, RetryAfter: 90 * time.Second, Msg: "HTTP 429"}, code: 429, typ: "rate_limit_error", retry: "90"},
		"429 without Retry-After": {err: &provider.UpstreamError{Status: 429, Msg: "HTTP 429: quota exceeded"}, code: 429, typ: "rate_limit_error", want: []string{"quota exceeded"}},
		"credential file":         {err: errors.New("read /home/x/secret.json: no such file"), code: 502, typ: "upstream_error", want: []string{"provider acc1", "planport-"}, notWant: []string{"/home/x"}, inLog: "/home/x"},
		"network error URL":       {err: errors.New(`Post "https://auth.example/oauth/token": dial tcp: refused`), code: 502, typ: "upstream_error", want: []string{"planport-"}, notWant: []string{"auth.example"}, inLog: "auth.example"},
		"request error":           {err: &provider.RequestError{Err: errors.New(`role "tool" not supported`)}, code: 400, typ: "invalid_request_error", want: []string{`role "tool" not supported`}},
		"request error late":      {err: &provider.RequestError{Err: errors.New("image URL not downloadable")}, late: true, code: 400, typ: "invalid_request_error", want: []string{"image URL not downloadable"}},
		"upstream 400":            {err: &provider.UpstreamError{Status: 400, Msg: "HTTP 400: invalid reasoning_effort"}, code: 400, typ: "invalid_request_error", want: []string{"invalid reasoning_effort"}},
		"upstream 401":            {err: &provider.UpstreamError{Status: 401, Msg: "HTTP 401: Incorrect API key sk-abc"}, code: 502, typ: "upstream_error", want: []string{"HTTP 401"}, notWant: []string{"sk-abc"}, inLog: "sk-abc"},
		"upstream 403":            {err: &provider.UpstreamError{Status: 403, Msg: "HTTP 403: key sk-abc is locked"}, code: 502, typ: "upstream_error", want: []string{"HTTP 403"}, notWant: []string{"sk-abc"}, inLog: "sk-abc"},
		"upstream 404 model":      {err: &provider.UpstreamError{Status: 404, Msg: "HTTP 404: model not found"}, code: 502, typ: "upstream_error", want: []string{"model not found"}},
		"wrapped deadline":        {err: fmt.Errorf("Post \"https://auth.example/x\": %w", context.DeadlineExceeded), code: 502, typ: "upstream_error", want: []string{"PLANPORT_TIMEOUT", "acc1", "planport-"}, notWant: []string{"auth.example"}, inLog: "auth.example"},
		"url.Error deadline":      {err: &url.Error{Op: "Post", URL: "https://auth.example/x", Err: context.DeadlineExceeded}, code: 502, typ: "upstream_error", want: []string{"PLANPORT_TIMEOUT", "acc1", "planport-"}, notWant: []string{"auth.example"}, inLog: "auth.example"},
		"client canceled":         {err: &url.Error{Op: "Post", URL: "https://auth.example/x", Err: context.Canceled}, code: 502, typ: "upstream_error", want: []string{"acc1", "planport-"}, notWant: []string{"auth.example", "PLANPORT_TIMEOUT"}, inLog: "auth.example"},
	} {
		for _, stream := range []bool{false, true} {
			logBuf.Reset()
			target, other := &fake{id: "acc1", err: tc.err, late: tc.late}, &fake{id: "acc2", text: "hello"}
			rec := call(t, New(provider.Registry{target, other}, ""), "POST", "/v1/chat/completions", chatBody("acc1", stream))
			var body struct {
				Error struct{ Message, Type string }
			}
			err := json.Unmarshal(rec.Body.Bytes(), &body)
			bad := err != nil || rec.Code != tc.code || body.Error.Type != tc.typ || rec.Header().Get("Retry-After") != tc.retry ||
				target.calls != 1 || other.calls != 0 || !strings.Contains(logBuf.String(), tc.inLog)
			for _, w := range tc.want {
				bad = bad || !strings.Contains(body.Error.Message, w)
			}
			for _, w := range tc.notWant {
				bad = bad || strings.Contains(rec.Body.String(), w)
			}
			if bad {
				t.Errorf("%s stream=%v: want %d %s Retry-After=%q, %q in message, not %q, log %q: got %d %q calls=%d/%d %s log=%q",
					name, stream, tc.code, tc.typ, tc.retry, tc.want, tc.notWant, tc.inLog, rec.Code, rec.Header().Get("Retry-After"), target.calls, other.calls, rec.Body, logBuf)
			}
		}
	}
}

// A stream opens with the role chunk even without text, then content, the
// final chunk and [DONE].
func TestStream(t *testing.T) {
	for _, text := range []string{"café — 世界", ""} {
		rec := call(t, New(provider.Registry{&fake{id: "acc1", text: text}}, ""), "POST", "/v1/chat/completions", chatBody("acc1", true))
		b := rec.Body.String()
		role, stop := strings.Index(b, `"delta":{"role":"assistant"}`), strings.Index(b, `"finish_reason":"stop"`)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" || role < 0 || stop < role ||
			strings.Contains(b, `"delta":{"content":"`+text+`"}`) != (text != "") || !strings.HasSuffix(b, "data: [DONE]\n\n") {
			t.Fatalf("text %q: bad stream: %d %s", text, rec.Code, b)
		}
	}
}

// Truncation surfaces as "length", not "stop"; an error after text ends the
// stream with an "error" chunk + [DONE].
func TestFinishReason(t *testing.T) {
	chat := func(p provider.Provider, stream bool) string {
		return call(t, New(provider.Registry{p}, ""), "POST", "/v1/chat/completions", chatBody("acc1", stream)).Body.String()
	}
	cut := &fake{id: "acc1", text: "partial", finish: "length"}
	if b := chat(cut, false); !strings.Contains(b, `"finish_reason":"length"`) {
		t.Fatalf("truncated non-stream must be length: %s", b)
	}
	if b := chat(cut, true); !strings.Contains(b, `"finish_reason":"length"`) || !strings.HasSuffix(b, "data: [DONE]\n\n") {
		t.Fatalf("truncated stream must be length: %s", b)
	}
	broken := &fake{id: "acc1", text: "partial", err: errors.New("network lost mid-stream"), late: true}
	if b := chat(broken, true); !strings.Contains(b, `"content":"partial"`) || !strings.Contains(b, `"finish_reason":"error"`) || !strings.HasSuffix(b, "data: [DONE]\n\n") {
		t.Fatalf("mid-stream error must end with error chunk + [DONE]: %s", b)
	}
	if b := chat(&fake{id: "acc1", text: "full"}, false); !strings.Contains(b, `"finish_reason":"stop"`) {
		t.Fatalf("normal reply must be stop: %s", b)
	}
}

// reasoning_effort and model reach the chosen provider verbatim in both modes.
func TestReasoningEffortReachesProvider(t *testing.T) {
	for _, stream := range []string{"false", "true"} {
		other := &fake{id: "acc1", text: "ok"}
		target := &fake{id: "acc2", text: "ok"}
		s := New(provider.Registry{other, target}, "")
		rec := call(t, s, "POST", "/v1/chat/completions", `{"provider":"acc2","model":"m-x","stream":`+stream+`,"reasoning_effort":"e-y","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != 200 || target.got.ReasoningEffort != "e-y" || target.got.Model != "m-x" || other.calls != 0 {
			t.Fatalf("stream=%s: acc2 must get reasoning_effort=e-y model=m-x, acc1 not called: %+v acc1=%d (HTTP %d)", stream, target.got, other.calls, rec.Code)
		}
	}
}

// Browsers (Origin, Sec-Fetch-Site other than "none") are 403; without a key
// so are foreign Hosts (DNS rebinding). With a key, local programs (loopback
// connection and Host) need none; network calls, even via the machine's own
// IP, need "Bearer <key>" (scheme case-insensitive, RFC 7235).
func TestAccessGuard(t *testing.T) {
	for _, tc := range []struct {
		key, remote, url, header string // remote "" = httptest's 192.0.2.1:1234; header "Name: value"
		want                     int
	}{
		{"", "", "http://127.0.0.1:18080/v1/chat/completions", "Origin: https://evil.example", 403},
		{"", "", "http://evil.example:18080/v1/chat/completions", "", 403},
		{"", "", "http://evil.example:18080/healthz", "", 403},
		{"", "", "http://127.0.0.1:18080/v1/chat/completions", "", 200},
		{"", "", "http://localhost:18080/v1/chat/completions", "", 200},
		{"", "", "http://[::1]:18080/v1/chat/completions", "", 200},
		{"", "", "http://127.0.0.1:18080/healthz", "Sec-Fetch-Site: cross-site", 403},
		{"", "", "http://127.0.0.1:18080/healthz", "Sec-Fetch-Site: same-site", 403},
		{"", "", "http://127.0.0.1:18080/healthz", "Sec-Fetch-Site: none", 200},
		{"", "", "http://127.0.0.1/healthz", "Host: ", 200}, // HTTP/1.0 without Host is not a browser
		{"k", "", "http://127.0.0.1/v1/chat/completions", "Authorization: Bearer k", 200},
		{"k", "", "http://127.0.0.1/v1/chat/completions", "Authorization: bearer k", 200},
		{"k", "", "http://127.0.0.1/v1/chat/completions", "Authorization: Bearer  k", 200},
		{"k", "", "http://127.0.0.1/v1/chat/completions", "Authorization: Bearer\tk", 200},
		{"k", "", "http://127.0.0.1/v1/chat/completions", "Authorization: k", 401},
		{"k", "", "http://127.0.0.1/v1/chat/completions", "Authorization: Bearer kk", 401},
		{"k", "", "http://127.0.0.1/v1/chat/completions", "Authorization: Bearer ", 401},
		{"k", "", "http://127.0.0.1/v1/chat/completions", "Authorization: Basic k", 401},
		{"k", "", "http://127.0.0.1/v1/chat/completions", "", 401},
		{"k", "127.0.0.1:5000", "http://127.0.0.1:18080/v1/chat/completions", "", 200},
		{"k", "[::1]:5000", "http://localhost:18080/v1/chat/completions", "", 200},
		{"k", "198.51.100.50:4444", "http://192.0.2.16:18080/v1/chat/completions", "", 401},
		{"k", "198.51.100.50:4444", "http://192.0.2.16:18080/v1/chat/completions", "Authorization: Bearer k", 200},
		{"k", "198.51.100.50:4444", "http://127.0.0.1:18080/v1/chat/completions", "", 401},
		{"k", "127.0.0.1:5000", "http://192.0.2.16:18080/v1/chat/completions", "", 401},
		{"k", "198.51.100.50:4444", "http://192.0.2.16:18080/v1/chat/completions", "Origin: http://evil.example", 403},
	} {
		method, body := "POST", chatBody("acc1", false)
		if strings.HasSuffix(tc.url, "/healthz") {
			method, body = "GET", ""
		}
		req := httptest.NewRequest(method, tc.url, strings.NewReader(body))
		if tc.remote != "" {
			req.RemoteAddr = tc.remote
		}
		if k, v, _ := strings.Cut(tc.header, ": "); k == "Host" {
			req.Host = v
		} else if k != "" {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		New(provider.Registry{&fake{id: "acc1", text: "ok"}}, tc.key).ServeHTTP(rec, req)
		if rec.Code != tc.want || (tc.want == 200 && method == "GET" && rec.Body.String() != "ok") {
			t.Errorf("key=%q %s → %s %q: want %d, got %d %s", tc.key, tc.remote, tc.url, tc.header, tc.want, rec.Code, rec.Body)
		}
	}
}

// streamProbe checks the provider sees emit errors and stops upstream at once.
type streamProbe struct {
	emitted int
	emitErr error
}

func (p *streamProbe) ID() string { return "probe" }
func (p *streamProbe) StreamChat(_ context.Context, _ provider.ChatRequest, emit func(provider.StreamEvent) error) error {
	for i := 0; i < 2; i++ {
		p.emitted++
		if err := emit(provider.StreamEvent{Text: "chunk"}); err != nil {
			p.emitErr = err
			return err
		}
	}
	return nil
}

type brokenStreamWriter struct {
	*httptest.ResponseRecorder
	writeErr, flushErr error
}

func (w *brokenStreamWriter) Write(b []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.ResponseRecorder.Write(b)
}
func (w *brokenStreamWriter) FlushError() error { return w.flushErr }

func TestStreamPropagatesClientErrors(t *testing.T) {
	sentinel := errors.New("client disconnected")
	for _, tc := range []struct {
		name               string
		writeErr, flushErr error
	}{{"write", sentinel, nil}, {"flush", nil, sentinel}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &streamProbe{}
			w := &brokenStreamWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: tc.writeErr, flushErr: tc.flushErr}
			New(provider.Registry{p}, "").ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1/v1/chat/completions", strings.NewReader(chatBody("probe", true))))
			if !errors.Is(p.emitErr, sentinel) || p.emitted != 1 {
				t.Fatalf("provider must stop on client error: emits=%d err=%v", p.emitted, p.emitErr)
			}
		})
	}
}

// GET /v1/providers never calls upstream; GET /v1/models adds object,
// owned_by and keeps created (strict OpenAI clients need it, 0 if absent).
func TestDiscoveryRoutes(t *testing.T) {
	p := &catalogProvider{fake: fake{id: "a"}, models: []provider.Model{{ID: "up-model", DisplayName: "Upstream model"}, {ID: "m2"}}}
	s := New(provider.Registry{p, &fake{id: "b"}}, "")
	rec := call(t, s, "GET", "/v1/providers", "")
	var inventory struct {
		Data []struct {
			ID              string `json:"id"`
			ModelsSupported bool   `json:"models_supported"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inventory); err != nil || rec.Code != 200 || p.lists != 0 || len(inventory.Data) != 2 ||
		inventory.Data[0].ID != "a" || !inventory.Data[0].ModelsSupported || inventory.Data[1].ModelsSupported {
		t.Fatalf("providers: %d calls=%d %s err=%v", rec.Code, p.lists, rec.Body, err)
	}
	rec = call(t, s, "GET", "/v1/models?provider=a", "")
	var catalog struct {
		Object   string           `json:"object"`
		Provider string           `json:"provider"`
		Data     []provider.Model `json:"data"`
	}
	var raw struct{ Data []map[string]json.RawMessage }
	if err := errors.Join(json.Unmarshal(rec.Body.Bytes(), &catalog), json.Unmarshal(rec.Body.Bytes(), &raw)); err != nil || rec.Code != 200 ||
		catalog.Object != "list" || catalog.Provider != "a" || len(catalog.Data) != 2 || catalog.Data[0].ID != "up-model" || catalog.Data[0].OwnedBy != "a" || catalog.Data[0].Object != "model" {
		t.Fatalf("models: %d %s err=%v", rec.Code, rec.Body, err)
	}
	for i, m := range raw.Data {
		if _, ok := m["created"]; !ok {
			t.Fatalf("data[%d] lacks created: %s", i, rec.Body)
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/v1/models", 400}, {"GET", "/v1/models?provider=unknown", 404}, {"GET", "/v1/models?provider=b", 501}, {"POST", "/v1/models?provider=a", 405},
		{"POST", "/v1/providers", 405}, {"GET", "/v1/chat/completions", 405}, {"GET", "/v1/nope", 404}} {
		if rec := call(t, s, tc.method, tc.path, ""); rec.Code != tc.status {
			t.Fatalf("%s %s: %d want %d", tc.method, tc.path, rec.Code, tc.status)
		}
	}
	for _, path := range []string{"/v1/models?provider=a", "/v1/providers"} {
		if rec := call(t, New(provider.Registry{p}, "secret"), "GET", path, ""); rec.Code != 401 {
			t.Fatalf("auth %s: %d", path, rec.Code)
		}
	}
}

// Listing errors: 429 + Retry-After as in chat, else 502 hiding paths (kept in
// the server log); never 400, since listing forwards nothing from the client.
func TestModelsErrors(t *testing.T) {
	logBuf := captureLog(t)
	for name, tc := range map[string]struct {
		err           error
		code          int
		retry, want   string
		hidden, inLog string
	}{
		"429":          {err: &provider.UpstreamError{Status: 429, RetryAfter: 7 * time.Second, Msg: "HTTP 429"}, code: 429, retry: "7", want: "HTTP 429"},
		"key file":     {err: errors.New("a: read key file: open /home/x/key.txt: permission denied"), code: 502, want: "provider a", hidden: "/home/x", inLog: "/home/x/key.txt"},
		"upstream 400": {err: &provider.UpstreamError{Status: 400, Msg: "HTTP 400: bad"}, code: 502, want: "HTTP 400: bad"},
	} {
		logBuf.Reset()
		p := &catalogProvider{fake: fake{id: "a"}, listErr: tc.err}
		rec := call(t, New(provider.Registry{p}, ""), "GET", "/v1/models?provider=a", "")
		b := rec.Body.String()
		if rec.Code != tc.code || rec.Header().Get("Retry-After") != tc.retry || !strings.Contains(b, tc.want) ||
			(tc.hidden != "" && strings.Contains(b, tc.hidden)) || !strings.Contains(logBuf.String(), tc.inLog) {
			t.Errorf("%s: want %d Retry-After=%q with %q, without %q: %d %q %s log=%q", name, tc.code, tc.retry, tc.want, tc.hidden, rec.Code, rec.Header().Get("Retry-After"), b, logBuf)
		}
	}
}
