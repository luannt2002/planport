package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luannt2002/planport/internal/provider"
	"github.com/luannt2002/planport/internal/server"
)

const okResult = `{"type":"result","is_error":false,"result":"ok"}`

func fakeCLI(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// printCLI is a provider whose CLI drains stdin and prints lines (NDJSON).
func printCLI(t *testing.T, lines ...string) *Provider {
	return New(Config{Name: "cc", Binary: fakeCLI(t, "cat >/dev/null\nprintf '%s\\n' '"+strings.Join(lines, "' '")+"'\n"), Timeout: time.Second})
}

// recordCLI is a provider whose CLI saves its args, env and stdin to files in
// the returned dir, then succeeds.
func recordCLI(t *testing.T) (*Provider, string) {
	dir := t.TempDir()
	script := "printf '%s\\n' \"$@\" > '" + dir + "/args'\nenv > '" + dir + "/env'\ncat > '" + dir + "/stdin'\nprintf '%s\\n' '" + okResult + "'\n"
	return New(Config{Name: "cc", Binary: fakeCLI(t, script), Timeout: time.Second}), dir
}

func request() provider.ChatRequest {
	return provider.ChatRequest{Model: "sonnet", Messages: []provider.ChatMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}}}
}

// chat runs req on p and discards the output.
func chat(p *Provider, req provider.ChatRequest) error {
	return p.StreamChat(context.Background(), req, func(provider.StreamEvent) error { return nil })
}

func TestStreamAndUsage(t *testing.T) {
	p := printCLI(t, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi — café"}}}`, `{"type":"assistant","message":{"content":[{"type":"text","text":"Hi — café"}]}}`, `{"type":"result","is_error":false,"result":"Hi — café","usage":{"input_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":2,"output_tokens":4}}`)
	var text string
	var usage provider.Usage
	err := p.StreamChat(context.Background(), request(), func(ev provider.StreamEvent) error {
		text += ev.Text
		if ev.Usage != nil {
			usage = *ev.Usage
		}
		return nil
	})
	if err != nil || text != "Hi — café" || usage.PromptTokens != 10 || usage.TotalTokens != 14 {
		t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
	}
}

// A stream without a result line, or with invalid JSON, must fail.
func TestFailureAndTruncation(t *testing.T) {
	for _, output := range []string{`{"type":"system"}`, `bad-json`} {
		if err := chat(printCLI(t, output), request()); err == nil {
			t.Fatalf("want error for %s", output)
		}
	}
}

func TestCancelAndEmitError(t *testing.T) {
	p := New(Config{Name: "cc", Binary: fakeCLI(t, "exec sleep 30\n"), Timeout: 30 * time.Millisecond})
	if err := chat(p, request()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	sentinel := errors.New("client disconnected")
	p = New(Config{Name: "cc", Binary: fakeCLI(t, `cat >/dev/null
printf '%s\n' '{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"a"}}}'
exec sleep 30
`), Timeout: time.Second})
	if err := p.StreamChat(context.Background(), request(), func(provider.StreamEvent) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("emit: %v", err)
	}
}

func TestRejectUnsupportedInput(t *testing.T) {
	p := New(Config{Name: "cc", Binary: "/does/not/exist", Timeout: time.Second})
	req := request()
	n := 10
	req.MaxTokens = &n
	if err := chat(p, req); !provider.ClientFault(err) {
		t.Fatalf("max_tokens: %v", err)
	}
	req = request()
	req.Messages[0].Content = json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`)
	if err := chat(p, req); !provider.ClientFault(err) {
		t.Fatalf("image: %v", err)
	}
}

func TestPromptHistoryAndToolIsolation(t *testing.T) {
	p, dir := recordCLI(t)
	req := request()
	req.Messages = append([]provider.ChatMessage{{Role: "system", Content: json.RawMessage(`"Be brief"`)}, {Role: "user", Content: json.RawMessage(`"My name is An"`)}, {Role: "assistant", Content: json.RawMessage(`"Hi An"`)}}, req.Messages...)
	if err := chat(p, req); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "args"))
	b, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	for _, want := range []string{"--tools\n\n", "--strict-mcp-config", "--no-session-persistence", "--system-prompt-file", "--model\nsonnet"} {
		if !strings.Contains(string(a), want) {
			t.Fatalf("args missing %q", want)
		}
	}
	if !strings.Contains(string(b), "My name is An") || !strings.Contains(string(b), "assistant") {
		t.Fatalf("history missing: %s", b)
	}
}

func TestInputMultimodal(t *testing.T) {
	for _, tc := range []struct{ name, content, kind string }{
		{"image", `[{"type":"text","text":"What is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]`, "image"},
		{"PDF", `[{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,aGk="}}]`, "document"},
		{"text file", `[{"type":"file","file":{"filename":"a.txt","file_data":"data:text/plain;base64,aGk="}}]`, "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := request()
			req.Messages[0].Content = json.RawMessage(tc.content)
			b, _, err := input(req)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Message struct {
					Content []struct {
						Type   string
						Text   string
						Source struct {
							Type      string
							MediaType string `json:"media_type"`
							Data      string
						}
					}
				}
			}
			if err = json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			c := got.Message.Content[len(got.Message.Content)-1]
			if c.Type != tc.kind {
				t.Fatalf("type=%q, want %q", c.Type, tc.kind)
			}
			if tc.kind == "text" {
				if !strings.Contains(c.Text, "hi") {
					t.Fatal("file content lost")
				}
			} else if c.Source.Type != "base64" || c.Source.Data != "aGk=" {
				t.Fatalf("source=%+v", c.Source)
			}
		})
	}
}

func TestLargeSystemPrompt(t *testing.T) {
	req := request()
	text, _ := json.Marshal(strings.Repeat("a", 180000))
	req.Messages = append([]provider.ChatMessage{{Role: "system", Content: text}}, req.Messages...)
	if err := chat(printCLI(t, okResult), req); err != nil {
		t.Fatal(err)
	}
}

func TestTimeoutKillsDescendants(t *testing.T) {
	p := New(Config{Name: "cc", Binary: fakeCLI(t, "sleep 2 &\nwait\n"), Timeout: 30 * time.Millisecond})
	start := time.Now()
	if err := chat(p, request()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout blocked by a child process: %s", elapsed)
	}
}

func TestEmptyDeltaDoesNotHideResult(t *testing.T) {
	p := printCLI(t, `{"type":"stream_event","event":{"delta":{"type":"text_delta","text":""}}}`, `{"type":"result","is_error":false,"result":"answer"}`)
	var text string
	if err := p.StreamChat(context.Background(), request(), func(ev provider.StreamEvent) error { text += ev.Text; return nil }); err != nil {
		t.Fatal(err)
	}
	if text != "answer" {
		t.Fatalf("result lost after an empty delta: %q", text)
	}
}

func TestResultIncludesDiagnostic(t *testing.T) {
	p := printCLI(t, `{"type":"result","is_error":true,"subtype":"error_during_execution","errors":["rate limited"]}`)
	if err := chat(p, request()); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("missing diagnostic: %v", err)
	}
}

// The CLI must not inherit PLANPORT_* (config loads .env, incl. PLANPORT_API_KEY,
// into the process env) or login/backend overrides, but keeps HOME and PATH.
func TestChildEnvFiltered(t *testing.T) {
	for k, v := range map[string]string{"PLANPORT_API_KEY": "k-secret", "PLANPORT_PROVIDERS": "claude", "ANTHROPIC_API_KEY": "x", "CLAUDE_CODE_USE_BEDROCK": "1", "CLAUDECODE": "1", "HOME": t.TempDir()} {
		t.Setenv(k, v)
	}
	p, dir := recordCLI(t)
	if err := chat(p, request()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	var home, path bool
	for _, kv := range strings.Split(string(raw), "\n") {
		for _, bad := range []string{"PLANPORT_", "ANTHROPIC_", "CLAUDE_CODE_", "CLAUDECODE="} {
			if strings.HasPrefix(kv, bad) {
				t.Errorf("child env leaked %q", kv)
			}
		}
		home = home || strings.HasPrefix(kv, "HOME=")
		path = path || strings.HasPrefix(kv, "PATH=")
	}
	if !home || !path {
		t.Fatalf("child env must keep HOME and PATH: HOME=%v PATH=%v", home, path)
	}
}

// An API error in the result keeps its HTTP status (429 lets the client back
// off, 400 is a client fault); without api_error_status it stays internal.
func TestResultAPIErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		status       int // 0 = not an UpstreamError
		client       bool
	}{
		{"429", `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"API Error: 429 rate limit"}`, 429, false},
		{"400", `{"type":"result","subtype":"success","is_error":true,"api_error_status":400,"result":"API Error: 400 bad"}`, 400, true},
		{"500", `{"type":"result","subtype":"success","is_error":true,"api_error_status":500,"result":"API Error: 500"}`, 500, false},
		{"null", `{"type":"result","subtype":"success","is_error":true,"api_error_status":null,"result":"quota"}`, 0, false},
		{"absent", `{"type":"result","subtype":"success","is_error":true,"result":"quota"}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := chat(printCLI(t, tc.result), request())
			if err == nil {
				t.Fatal("is_error result must fail")
			}
			var up *provider.UpstreamError
			got := 0
			if errors.As(err, &up) {
				got = up.Status
			}
			if got != tc.status || provider.ClientFault(err) != tc.client {
				t.Fatalf("status=%d (want %d) ClientFault=%v (want %v): %v", got, tc.status, provider.ClientFault(err), tc.client, err)
			}
		})
	}
}

func TestResultStopReason(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"max_tokens", `,"stop_reason":"max_tokens"`, "length"},
		{"refusal", `,"stop_reason":"refusal"`, "content_filter"},
		{"end_turn", `,"stop_reason":"end_turn"`, ""},
		{"null", `,"stop_reason":null`, ""},
		{"absent", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := printCLI(t, `{"type":"result","is_error":false,"result":"ok"`+tc.extra+`}`)
			finish := ""
			err := p.StreamChat(context.Background(), request(), func(ev provider.StreamEvent) error {
				if ev.Finish != "" {
					finish = ev.Finish
				}
				return nil
			})
			if err != nil || finish != tc.want {
				t.Fatalf("finish=%q, want %q, err=%v", finish, tc.want, err)
			}
		})
	}
}

// A CLI that dies early (not logged in, bad flag) logs its stderr tail on the
// server but never returns it to the client (it may hold paths or host info).
func TestStderrLoggedNotReturned(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	p := New(Config{Name: "cc", Binary: fakeCLI(t, "cat >/dev/null\necho boom-xyz >&2\nexit 1\n"), Timeout: time.Second})
	if err := chat(p, request()); err == nil || strings.Contains(err.Error(), "boom-xyz") {
		t.Fatalf("client error must not contain stderr: %v", err)
	}
	if !strings.Contains(buf.String(), "boom-xyz") {
		t.Fatalf("server log must contain the stderr tail, got %q", buf.String())
	}
}

func TestStderrTailBounded(t *testing.T) {
	w := &tailWriter{max: 8}
	w.Write([]byte("0123456789"))
	w.Write([]byte("ab"))
	if string(w.buf) != "456789ab" {
		t.Fatalf("tail = %q, want %q", w.buf, "456789ab")
	}
}

// Unknown reasoning_effort values fail with 400 before the CLI starts, so
// arbitrary input never becomes a command-line flag.
func TestEffortFlag(t *testing.T) {
	p, dir := recordCLI(t)
	args := filepath.Join(dir, "args")
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max", "bogus", "--model"} {
		os.Remove(args)
		req := request()
		req.ReasoningEffort = effort
		err := chat(p, req)
		a, _ := os.ReadFile(args)
		switch effort {
		case "bogus", "--model":
			if !provider.ClientFault(err) || a != nil {
				t.Fatalf("effort %q must fail with 400 before the CLI runs: err=%v args=%q", effort, err, a)
			}
		case "":
			if err != nil || strings.Contains(string(a), "--effort") {
				t.Fatalf("no effort must mean no flag: err=%v args=%q", err, a)
			}
		default:
			if err != nil || !strings.Contains(string(a), "--effort\n"+effort+"\n") {
				t.Fatalf("effort %q must become --effort %s: err=%v args=%q", effort, effort, err, a)
			}
		}
	}
}

// TestHTTP runs the real HTTP server on loopback; only the CLI is faked.
func TestHTTP(t *testing.T) {
	p := printCLI(t, `{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"Hello — café"}}}`, `{"type":"result","is_error":false,"result":"Hello — café","usage":{"input_tokens":3,"output_tokens":2}}`)
	srv := httptest.NewServer(server.New(provider.Registry{p}, "test-key"))
	defer srv.Close()
	for _, tc := range []struct {
		name, content string
		stream        bool
		status        int
	}{
		{"text", `"hi"`, false, 200},
		{"stream", `"hi"`, true, 200},
		{"file", `[{"type":"file","file":{"filename":"a.txt","file_data":"data:text/plain;base64,aGk="}}]`, false, 200},
		{"image", `[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]`, false, 200},
		{"PDF", `[{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,aGk="}}]`, false, 200},
		{"image URL", `[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`, false, 400},
		{"bad base64", `[{"type":"image_url","image_url":{"url":"data:image/png;base64,???"}}]`, false, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := request()
			req.Provider = "cc"
			req.Stream = tc.stream
			req.Messages[0].Content = json.RawMessage(tc.content)
			payload, _ := json.Marshal(req)
			r, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(payload))
			r.Header.Set("Authorization", "Bearer test-key")
			r.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status {
				t.Fatalf("status=%d body=%s", resp.StatusCode, b)
			}
			if tc.status != 200 {
				return
			}
			if tc.stream {
				if !strings.Contains(string(b), "[DONE]") || !strings.Contains(string(b), "Hello — café") {
					t.Fatalf("SSE=%s", b)
				}
			} else {
				var got struct {
					Choices []struct{ Message struct{ Content string } }
					Usage   provider.Usage
				}
				if err := json.Unmarshal(b, &got); err != nil || len(got.Choices) != 1 || got.Choices[0].Message.Content != "Hello — café" || got.Usage.TotalTokens != 5 {
					t.Fatalf("completion=%s err=%v", b, err)
				}
			}
		})
	}
}
