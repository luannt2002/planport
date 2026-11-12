package provider

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"
)

// maxEventBytes is the largest SSE event accepted (reasoning can be long); a
// memory safety limit, not deployment config.
const maxEventBytes = 4 << 20

// StreamSSE sends req, requires HTTP 200 (else *UpstreamError, label prefixing
// the message), then decodes each SSE "data:" payload into a StreamEvent and
// emits it. EOF before a finish event (non-empty Finish) or "[DONE]" is an
// error, so a cut stream is never reported as complete.
func StreamSSE(client *http.Client, req *http.Request, label string,
	decode func(data []byte) (StreamEvent, error), emit func(StreamEvent) error) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return NewUpstreamError(resp, label)
	}

	finished := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
	for sc.Scan() {
		// "event:" lines are skipped: the JSON payload carries its own type.
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		data = strings.TrimSpace(data)
		if !ok || data == "" {
			continue
		}
		if data == "[DONE]" {
			return nil
		}
		ev, err := decode([]byte(data))
		if err != nil {
			return err
		}
		finished = finished || ev.Finish != ""
		if err := emit(ev); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	if !finished {
		return fmt.Errorf("%s: stream ended before completion", label)
	}
	return nil
}
