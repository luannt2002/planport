package provider

// Typed errors let the server pick the HTTP status returned to the client.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

const (
	maxErrorBody = 1 << 20 // most bytes read from an error response body (memory safety)
	// ErrSnippet is how many body bytes go into an error message.
	ErrSnippet = 300
)

// UpstreamError is a non-200 upstream response received before streaming started.
type UpstreamError struct {
	Status     int
	RetryAfter time.Duration // 0 if upstream sent no wait time
	Msg        string
}

func (e *UpstreamError) Error() string { return e.Msg }

// NewUpstreamError reads a non-200 response into an *UpstreamError prefixed with label
// (e.g. "codex /responses"); the wait comes from Retry-After, or the body for a bare 429.
func NewUpstreamError(resp *http.Response, label string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	wait := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if wait == 0 && resp.StatusCode == http.StatusTooManyRequests {
		// Codex sends the quota reset in the body instead of a header.
		var b struct {
			Error struct {
				ResetsInSeconds int64 `json:"resets_in_seconds"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &b) == nil {
			wait = seconds(b.Error.ResetsInSeconds)
		}
	}
	return &UpstreamError{
		Status:     resp.StatusCode,
		RetryAfter: wait,
		Msg:        fmt.Sprintf("%s HTTP %d: %s", label, resp.StatusCode, Truncate(raw, ErrSnippet)),
	}
}

// RequestError is a client request that cannot be translated for the backend
// (e.g. an unknown role); it maps to 400.
type RequestError struct{ Err error }

func (e *RequestError) Error() string { return e.Err.Error() }
func (e *RequestError) Unwrap() error { return e.Err }

// ClientFault reports whether err is the client's fault (a RequestError or an
// upstream 400): the client gets 400, which SDKs do not retry.
func ClientFault(err error) bool {
	var re *RequestError
	var up *UpstreamError
	return errors.As(err, &re) || errors.As(err, &up) && up.Status == http.StatusBadRequest
}

// parseRetryAfter reads Retry-After as seconds or an HTTP date; anything else is 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if s, err := strconv.ParseInt(v, 10, 64); err == nil {
		return seconds(s)
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// seconds converts s to a Duration; non-positive or overflowing values are 0.
func seconds(s int64) time.Duration {
	if s <= 0 || s > math.MaxInt64/int64(time.Second) {
		return 0
	}
	return time.Duration(s) * time.Second
}

// Truncate cuts b to at most n bytes for an error message, backing up to a
// rune start so no UTF-8 character is split.
func Truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	return string(b[:n]) + "…"
}
