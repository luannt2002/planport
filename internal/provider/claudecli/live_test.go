package claudecli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/luannt2002/planport/internal/provider"
	"github.com/luannt2002/planport/internal/server"
)

// TestLiveClaudeHTTP is opt-in: it spends the Claude Code account quota.
func TestLiveClaudeHTTP(t *testing.T) {
	if os.Getenv("PLANPORT_CLAUDE_LIVE") != "1" {
		t.Skip("set PLANPORT_CLAUDE_LIVE=1 to call the real model")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(provider.Registry{New(Config{Name: "claude", Binary: binary, Timeout: 90 * time.Second})}, "live-test"))
	defer srv.Close()
	client := &http.Client{Timeout: 100 * time.Second}
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, img); err != nil {
		t.Fatal(err)
	}
	imageContent, _ := json.Marshal([]any{map[string]any{"type": "text", "text": "What is the dominant color? Reply with just the English color name."}, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData.Bytes())}}})
	fileContent, _ := json.Marshal([]any{map[string]any{"type": "text", "text": "Read the attached text file. Reply with only the value of verification_code."}, map[string]any{"type": "file", "file": map[string]string{"filename": "check.txt", "file_data": "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("verification_code=FILE_CEDAR_8219\n"))}}})
	for _, tc := range []struct {
		name    string
		content json.RawMessage
		stream  bool
		history bool
		want    string
	}{
		{"text", json.RawMessage(`"Reply exactly PLANPORT_OK"`), false, false, "PLANPORT_OK"},
		{"stream", json.RawMessage(`"Reply exactly STREAM_OK"`), true, false, "STREAM_OK"},
		{"text_file", fileContent, false, false, "FILE_CEDAR_8219"},
		{"image", imageContent, false, false, "red"},
		{"history", json.RawMessage(`"What was my verification code? Reply with only that code."`), false, true, "HISTORY_BIRCH_4872"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := provider.ChatRequest{Provider: "claude", Model: "sonnet", Stream: tc.stream, Messages: []provider.ChatMessage{{Role: "user", Content: tc.content}}}
			if tc.history {
				req.Messages = append([]provider.ChatMessage{{Role: "user", Content: json.RawMessage(`"My verification code is HISTORY_BIRCH_4872."`)}, {Role: "assistant", Content: json.RawMessage(`"I will remember it."`)}}, req.Messages...)
			}
			payload, _ := json.Marshal(req)
			r, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(payload))
			r.Header.Set("Authorization", "Bearer live-test")
			r.Header.Set("Content-Type", "application/json")
			start := time.Now()
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
			}
			var answer string
			if tc.stream {
				if !strings.Contains(string(b), "[DONE]") {
					t.Fatalf("missing [DONE]: %s", b)
				}
				for _, line := range strings.Split(string(b), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var chunk struct {
						Choices []struct{ Delta struct{ Content string } }
					}
					if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) == nil {
						for _, c := range chunk.Choices {
							answer += c.Delta.Content
						}
					}
				}
			} else {
				var out struct {
					Choices []struct{ Message struct{ Content string } }
				}
				if err := json.Unmarshal(b, &out); err != nil {
					t.Fatal(err)
				}
				if len(out.Choices) != 1 {
					t.Fatalf("response=%s", b)
				}
				answer = out.Choices[0].Message.Content
			}
			if !strings.EqualFold(strings.TrimSpace(answer), tc.want) {
				t.Fatalf("answer=%q, want %q", answer, tc.want)
			}
			t.Logf("HTTP 200, answer=%q, duration=%s", answer, time.Since(start).Round(time.Millisecond))
		})
	}
}
