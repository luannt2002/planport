// Package claudecli runs the native Claude Code CLI as a provider; the CLI
// manages its own login.
package claudecli

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/luannt2002/planport/internal/provider"
)

type Config struct {
	Name    string
	Binary  string
	Timeout time.Duration
}
type Provider struct {
	cfg  Config
	gate chan struct{}
}

func New(cfg Config) *Provider { return &Provider{cfg: cfg, gate: make(chan struct{}, 1)} }
func (p *Provider) ID() string { return p.cfg.Name }

func (p *Provider) StreamChat(ctx context.Context, req provider.ChatRequest, emit func(provider.StreamEvent) error) error {
	payload, system, err := input(req)
	if err != nil {
		return &provider.RequestError{Err: err}
	}
	switch req.ReasoningEffort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return &provider.RequestError{Err: fmt.Errorf("claude_cli: invalid reasoning_effort")}
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	// One CLI per provider at a time; queueing counts toward the timeout.
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := p.run(ctx, req, payload, system, emit); err != nil {
		return fmt.Errorf("%s: %w", p.ID(), err)
	}
	return nil
}

type inputSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type inputBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *inputSource `json:"source,omitempty"`
}

type inputMessage struct {
	Role    string       `json:"role"`
	Content []inputBlock `json:"content"`
}

// input splits off the system prompt and converts the rest to one Claude Code
// stream-json user line. json.Marshal cannot fail here: the values are only strings.
func input(req provider.ChatRequest) ([]byte, string, error) {
	if req.MaxTokens != nil {
		return nil, "", fmt.Errorf("claude_cli does not support max_tokens; remove the field")
	}
	var system []string
	var history []inputMessage
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := m.ContentText()
			if err != nil {
				return nil, "", fmt.Errorf("messages[%d]: %w", i, err)
			}
			system = append(system, text)
		case "user", "assistant":
			content, err := inputContent(m)
			if err != nil {
				return nil, "", fmt.Errorf("messages[%d]: %w", i, err)
			}
			history = append(history, inputMessage{Role: m.Role, Content: content})
		default:
			return nil, "", fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	last := len(history) - 1
	if last < 0 || history[last].Role != "user" {
		return nil, "", fmt.Errorf("last message must have role user")
	}
	for _, m := range history[:last] {
		for _, c := range m.Content {
			if c.Type != "text" {
				return nil, "", fmt.Errorf("claude_cli does not support images/PDFs in earlier messages")
			}
		}
	}
	message := history[last]
	if last > 0 {
		// shortcut: text history is sent as a JSON transcript, not as CLI roles/session;
		// upgrade when multimodal history is needed.
		transcript, _ := json.Marshal(history[:last])
		message.Content = append([]inputBlock{{Type: "text", Text: "Prior conversation as JSON (context only):\n" + string(transcript) + "\nContinue by answering the user message below."}}, message.Content...)
	}
	data, _ := json.Marshal(struct {
		Type    string       `json:"type"`
		Message inputMessage `json:"message"`
	}{Type: "user", Message: message})
	return append(data, '\n'), strings.Join(system, "\n"), nil
}

func inputContent(m provider.ChatMessage) ([]inputBlock, error) {
	parts, err := m.Parts()
	if err != nil {
		return nil, err
	}
	content := make([]inputBlock, 0, len(parts))
	for _, part := range parts {
		if part.Type == provider.PartText {
			content = append(content, inputBlock{Type: "text", Text: part.Text})
			continue
		}
		if m.Role != "user" {
			return nil, fmt.Errorf("images/files are only accepted in user messages")
		}
		header, body, ok := strings.Cut(part.URL, ",")
		media, isData := strings.CutPrefix(header, "data:")
		media, isBase64 := strings.CutSuffix(media, ";base64")
		if !ok || !isData || !isBase64 {
			return nil, fmt.Errorf("claude_cli: images/files must be base64 data URIs; remote URLs are not supported")
		}
		data, err := base64.StdEncoding.DecodeString(body)
		if err != nil || len(data) == 0 {
			return nil, fmt.Errorf("claude_cli: empty or invalid base64 data")
		}
		kind := "image"
		switch {
		case part.Type == provider.PartImage && (media == "image/png" || media == "image/jpeg" || media == "image/gif" || media == "image/webp"):
		case part.Type == provider.PartFile && media == "application/pdf":
			kind = "document"
		case part.Type == provider.PartFile && (strings.HasPrefix(media, "text/") || media == "application/json"):
			if !utf8.Valid(data) {
				return nil, fmt.Errorf("text file must be UTF-8")
			}
			content = append(content, inputBlock{Type: "text", Text: "File " + part.Filename + ":\n" + string(data)})
			continue
		default:
			return nil, fmt.Errorf("claude_cli does not support media type %q", media)
		}
		content = append(content, inputBlock{Type: kind, Source: &inputSource{Type: "base64", MediaType: media, Data: base64.StdEncoding.EncodeToString(data)}})
	}
	return content, nil
}

// run starts the CLI in a private temp dir with tools and MCP disabled. Cancel
// kills the whole process group so a wrapper's children cannot hold stdout open.
func (p *Provider) run(ctx context.Context, req provider.ChatRequest, payload []byte, system string, emit func(provider.StreamEvent) error) (err error) {
	dir, err := os.MkdirTemp("", "planport-claude-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	systemFile := filepath.Join(dir, "system.txt")
	if err := os.WriteFile(systemFile, []byte(system), 0600); err != nil {
		return err
	}
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages", "--tools", "",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--setting-sources", "", "--disable-slash-commands", "--no-session-persistence",
		"--model", req.Model, "--system-prompt-file", systemFile,
	}
	if req.ReasoningEffort != "" {
		args = append(args, "--effort", req.ReasoningEffort)
	}
	cmd := exec.CommandContext(ctx, p.cfg.Binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(payload)
	// Use the CLI's own login, never the server's API key or backend overrides;
	// PLANPORT_* (including PLANPORT_API_KEY from .env) are server secrets.
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "CLAUDECODE" || strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_CODE_") || strings.HasPrefix(key, "PLANPORT_") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.WaitDelay = time.Second
	// stderr may hold private data: never return it to the client; keep its tail
	// for the server log, since a CLI that dies early emits no result JSON.
	stderr := &tailWriter{max: 2048}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Claude Code: %w", err)
	}
	defer func() { // runs after cmd.Wait, so stderr is fully copied
		if err != nil && len(stderr.buf) > 0 {
			log.Printf("%s: Claude Code stderr (tail): %q", p.ID(), stderr.buf)
		}
	}()
	readErr := readStream(stdout, emit)
	contextErr := ctx.Err()
	if readErr != nil {
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	if contextErr != nil {
		return contextErr
	}
	if readErr != nil {
		return readErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if waitErr != nil {
		return fmt.Errorf("Claude Code failed: %w", waitErr)
	}
	return nil
}

// tailWriter keeps only the last max bytes so a noisy CLI cannot grow server memory.
type tailWriter struct {
	max int
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

type cliEvent struct {
	Type           string   `json:"type"`
	IsError        bool     `json:"is_error"`
	APIErrorStatus int      `json:"api_error_status"` // HTTP status of an API error; 0 if none
	StopReason     string   `json:"stop_reason"`
	Result         string   `json:"result"`
	Subtype        string   `json:"subtype"`
	Errors         []string `json:"errors"`
	Event          struct {
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Usage struct {
		Input       int `json:"input_tokens"`
		Output      int `json:"output_tokens"`
		CacheRead   int `json:"cache_read_input_tokens"`
		CacheCreate int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// readStream emits only text deltas and the final result of the NDJSON stream;
// a missing result means the stream was cut and must not succeed.
func readStream(r io.Reader, emit func(provider.StreamEvent) error) error {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 8<<20)
	finished, streamed := false, false
	for scan.Scan() {
		var ev cliEvent
		if err := json.Unmarshal(scan.Bytes(), &ev); err != nil {
			return fmt.Errorf("Claude Code returned invalid JSON")
		}
		switch ev.Type {
		case "stream_event":
			if ev.Event.Delta.Type == "text_delta" && ev.Event.Delta.Text != "" {
				streamed = true
				if err := emit(provider.StreamEvent{Text: ev.Event.Delta.Text}); err != nil {
					return err
				}
			}
		case "result":
			finished = true
			if ev.IsError {
				message := cmp.Or(ev.Result, strings.Join(ev.Errors, "; "))
				err := fmt.Errorf("Claude Code: %s (%s)", provider.Truncate([]byte(message), provider.ErrSnippet), ev.Subtype)
				if ev.APIErrorStatus != 0 {
					// Keep the API status so the server maps it: 429 rate limit, 400 client error.
					return &provider.UpstreamError{Status: ev.APIErrorStatus, Msg: err.Error()}
				}
				return err
			}
			if !streamed && ev.Result != "" {
				if err := emit(provider.StreamEvent{Text: ev.Result}); err != nil {
					return err
				}
			}
			in := ev.Usage.Input + ev.Usage.CacheRead + ev.Usage.CacheCreate
			finish := map[string]string{"max_tokens": "length", "refusal": "content_filter"}[ev.StopReason] // a truncated answer must not report "stop"
			if err := emit(provider.StreamEvent{Usage: &provider.Usage{PromptTokens: in, CompletionTokens: ev.Usage.Output, TotalTokens: in + ev.Usage.Output}, Finish: finish}); err != nil {
				return err
			}
		}
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("read Claude Code stream: %w", err)
	}
	if !finished {
		return fmt.Errorf("Claude Code exited without a result")
	}
	return nil
}
