// Package server serves POST /v1/chat/completions (OpenAI shape), GET
// /v1/providers, GET /v1/models and /healthz. It calls exactly the provider the
// client names; upstream always streams, non-stream replies are buffered.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/luannt2002/planport/internal/provider"
)

// maxChatBody bounds memory (bodies are read whole) yet fits a few base64 files; a var for tests.
var maxChatBody int64 = 64 << 20

// Bodies over largeBody (or of unknown size) cost several times their size in RAM until the
// reply ends, so at most largeSlots of them are handled at once.
const (
	largeBody  = 8 << 20
	largeSlots = 2
)

// bodyReadTimeout bounds reading the body only: a slow sender must not pin the connection (and
// shutdown), while a ReadTimeout would also cut long streams; a var for tests.
var bodyReadTimeout = 5 * time.Minute

// Server is the http.Handler for the configured providers.
type Server struct {
	providers provider.Registry
	apiKey    string
	reqCount  atomic.Int64
	large     chan struct{} // slots for large bodies
}

// New returns a Server; an empty apiKey disables the key check.
func New(providers provider.Registry, apiKey string) *Server {
	return &Server{providers: providers, apiKey: apiKey, large: make(chan struct{}, largeSlots)}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// No web page may spend the plan: block browsers (Sec-Fetch-Site covers no-cors loads; "none" = typed
	// URL) and, without a key, foreign Hosts (DNS rebinding); with a key, authed checks them.
	if site := r.Header.Get("Sec-Fetch-Site"); r.Header.Get("Origin") != "" || (site != "" && site != "none") || (s.apiKey == "" && !loopbackHost(r.Host)) {
		writeErr(w, http.StatusForbidden, "planport only accepts requests from programs (no Origin / Sec-Fetch-Site); without an API key Host must be loopback", "forbidden")
		return
	}
	var method string
	var handle http.HandlerFunc
	switch r.URL.Path {
	case "/healthz":
		_, _ = io.WriteString(w, "ok")
		return
	case "/v1/chat/completions":
		method, handle = http.MethodPost, s.handleChat
	case "/v1/models":
		method, handle = http.MethodGet, s.handleModels
	case "/v1/providers":
		method, handle = http.MethodGet, s.handleProviders
	default:
		writeErr(w, http.StatusNotFound, "route "+r.URL.Path+" not found", "not_found")
		return
	}
	if r.Method != method {
		writeErr(w, http.StatusMethodNotAllowed, "use "+method, "invalid_request_error")
		return
	}
	if !s.authed(r) {
		writeErr(w, http.StatusUnauthorized, "invalid or missing api key", "auth")
		return
	}
	handle(w, r)
}

// authed: with a key, calls not from this machine need "Bearer <key>" (scheme case-insensitive, RFC 7235).
// This machine = loopback connection AND Host; a same-machine proxy would make network calls look local.
func (s *Server) authed(r *http.Request) bool {
	remote, _, _ := net.SplitHostPort(r.RemoteAddr)
	if s.apiKey == "" || (net.ParseIP(remote).IsLoopback() && loopbackHost(r.Host)) {
		return true
	}
	f := strings.Fields(r.Header.Get("Authorization"))
	return len(f) == 2 && strings.EqualFold(f[0], "Bearer") && subtle.ConstantTimeCompare([]byte(f[1]), []byte(s.apiKey)) == 1
}

// loopbackHost: Host is 127.0.0.1 / ::1 / localhost (any port) or empty.
func loopbackHost(hostport string) bool {
	if hostport == "" {
		return true // HTTP/1.0 without Host; browsers always send one
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	tooLarge := func() {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body too large (max %d MiB)", maxChatBody>>20), "invalid_request_error")
	}
	if r.ContentLength > maxChatBody {
		// Refuse a declared oversize body without storing it (or taking a slot). A client already sending
		// gets it drained, not kept (bounded), so it reads the 413 instead of a reset; one that waits for
		// "100 Continue" gets the 413 at once and never uploads.
		if !strings.EqualFold(r.Header.Get("Expect"), "100-continue") {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
			_, _ = io.CopyN(io.Discard, r.Body, 2*maxChatBody)
		}
		tooLarge()
		return
	}
	if r.ContentLength < 0 || r.ContentLength > largeBody {
		select {
		case s.large <- struct{}{}:
		case <-r.Context().Done():
			return // e.g. an HTTP/2 client gone; HTTP/1 notices a gone client only after the body is read (Shutdown does not cancel it)
		}
		// Free the slot, then hand the body's pages back to the OS without delaying the reply.
		defer func() { <-s.large; go debug.FreeOSMemory() }()
	}
	// Read the whole body first: Unmarshal rejects trailing data that a Decoder
	// would silently ignore, and trailing data past the cap becomes a 413.
	var body bytes.Buffer
	// Sized from the declared length only with a slot: otherwise idle clients could pin that much each.
	if r.ContentLength > largeBody {
		body.Grow(int(r.ContentLength) + bytes.MinRead) // + MinRead: ReadFrom regrows a full buffer before it sees EOF
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout)) // unsupported (e.g. ResponseRecorder): no deadline
	_, err := body.ReadFrom(http.MaxBytesReader(w, r.Body, maxChatBody))
	if err == nil { // after an error it stays, so the server's discard of the unread rest cannot hang
		_ = rc.SetReadDeadline(time.Time{}) // a stream must not inherit it (HTTP/1 also clears it at EOF)
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) { // undeclared (chunked) size past the cap
		tooLarge()
		return
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		writeErr(w, http.StatusRequestTimeout, "request body read timed out", "invalid_request_error")
		return
	}
	var req provider.ChatRequest
	if err == nil {
		err = json.Unmarshal(body.Bytes(), &req)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	if req.Provider == "" || req.Model == "" || len(req.Messages) == 0 {
		writeErr(w, http.StatusBadRequest, "missing provider, model or messages", "invalid_request_error")
		return
	}
	p, err := s.providers.ByID(req.Provider)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error(), "not_found")
		return
	}
	reqID := fmt.Sprintf("planport-%d", s.reqCount.Add(1))
	if req.Stream {
		serveStream(w, r, p, req, reqID)
	} else {
		serveBuffered(w, r, p, req, reqID)
	}
}

func serveBuffered(w http.ResponseWriter, r *http.Request, p provider.Provider, req provider.ChatRequest, reqID string) {
	start := time.Now()
	var text strings.Builder
	var usage provider.Usage
	finish := "stop" // replaced when the backend reports truncation
	err := p.StreamChat(r.Context(), req, func(ev provider.StreamEvent) error {
		if ev.Usage != nil {
			usage = usage.Add(*ev.Usage)
		}
		if ev.Finish != "" {
			finish = ev.Finish
		}
		text.WriteString(ev.Text)
		return nil
	})
	if err != nil {
		log.Printf("chat %s %s/%q: %v", reqID, p.ID(), req.Model, err) // %q: the model is client text
		writeProviderErr(w, err, p.ID(), reqID, true)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(completion{
		ID: reqID, Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []choiceOut{{Message: messageOut{Role: "assistant", Content: text.String()}, FinishReason: finish}},
		Usage:   usage,
	})
	log.Printf("chat %s %s/%q done in %s (usage %+v)", reqID, p.ID(), req.Model, time.Since(start).Round(time.Millisecond), usage)
}

func serveStream(w http.ResponseWriter, r *http.Request, p provider.Provider, req provider.ChatRequest, reqID string) {
	start := time.Now()
	rc := http.NewResponseController(w)
	started := false
	var clientErr error // a write or flush failed: the client is gone
	// send opens the stream on first use: headers wait for text so earlier errors keep a real HTTP status.
	send := func(content, finish string) error {
		if !started {
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			clientErr = writeChunk(w, reqID, req.Model, deltaBody{Role: "assistant"}, "")
		}
		if clientErr == nil {
			clientErr = writeChunk(w, reqID, req.Model, deltaBody{Content: content}, finish)
		}
		if clientErr == nil {
			clientErr = rc.Flush()
		}
		return clientErr
	}
	var usage provider.Usage
	finish := "stop" // replaced when the backend reports truncation
	err := p.StreamChat(r.Context(), req, func(ev provider.StreamEvent) error {
		if ev.Usage != nil {
			usage = usage.Add(*ev.Usage)
		}
		if ev.Finish != "" {
			finish = ev.Finish
		}
		if ev.Text == "" {
			return nil
		}
		return send(ev.Text, "")
	})
	if clientErr != nil {
		log.Printf("chat %s: client disconnected: %v", reqID, clientErr)
		return
	}
	if err != nil {
		log.Printf("chat %s %s/%q: %v", reqID, p.ID(), req.Model, err) // %q: the model is client text
		if !started {
			writeProviderErr(w, err, p.ID(), reqID, true)
			return
		}
		finish = "error" // status already sent
	}
	if send("", finish) != nil {
		return
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil || rc.Flush() != nil {
		return
	}
	log.Printf("chat %s %s/%q done in %s (usage %+v)", reqID, p.ID(), req.Model, time.Since(start).Round(time.Millisecond), usage)
}

// writeChunk writes one "data:" event; an empty finish means finish_reason null.
func writeChunk(w io.Writer, reqID, model string, delta deltaBody, finish string) error {
	c := chunkChoice{Delta: delta}
	if finish != "" {
		c.FinishReason = &finish
	}
	line, _ := json.Marshal(chunk{ID: reqID, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: model, Choices: []chunkChoice{c}}) // strings and ints: cannot fail
	_, err := fmt.Fprintf(w, "data: %s\n\n", line)
	return err
}

// handleProviders never calls upstream, so it answers even when one is down.
func (s *Server) handleProviders(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		ID              string `json:"id"`
		ModelsSupported bool   `json:"models_supported"`
	}
	list := make([]entry, 0, len(s.providers))
	for _, p := range s.providers {
		_, canList := p.(provider.ModelLister)
		list = append(list, entry{p.ID(), canList})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]entry{"data": list})
}

// handleModels sets owned_by because upstream doesn't know planport's provider names.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("provider")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing provider query parameter (?provider=<name>, see GET /v1/providers)", "invalid_request_error")
		return
	}
	p, err := s.providers.ByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error(), "not_found")
		return
	}
	lister, ok := p.(provider.ModelLister)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "provider "+p.ID()+" does not support listing models", "not_implemented")
		return
	}
	models, err := lister.Models(r.Context())
	if err != nil {
		reqID := fmt.Sprintf("planport-%d", s.reqCount.Add(1))
		log.Printf("models %s %s: %v", reqID, p.ID(), err)
		writeProviderErr(w, err, p.ID(), reqID, false)
		return
	}
	if models == nil {
		models = []provider.Model{} // "data": [], not null
	}
	for i := range models {
		models[i].Object, models[i].OwnedBy = "model", p.ID()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Object   string           `json:"object"`
		Provider string           `json:"provider"`
		Data     []provider.Model `json:"data"`
	}{"list", p.ID(), models})
}

func writeErr(w http.ResponseWriter, status int, message, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{"error": {"message": message, "type": typ}})
}

// writeProviderErr: 400 only for a forwarded client request (GET /v1/models forwards nothing), 429, else
// 502. The server may be network-exposed, so 502 hides internal errors (paths, token URLs) and upstream
// 401/403 bodies (some echo the key) behind pid + reqID, which find them in the log.
func writeProviderErr(w http.ResponseWriter, err error, pid, reqID string, forwarded bool) {
	var up *provider.UpstreamError
	isUp := errors.As(err, &up)
	status, typ, msg := http.StatusBadGateway, "upstream_error", err.Error()
	switch {
	case forwarded && provider.ClientFault(err):
		status, typ = http.StatusBadRequest, "invalid_request_error"
	case isUp && up.Status == http.StatusTooManyRequests:
		status, typ = http.StatusTooManyRequests, "rate_limit_error"
		if up.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(up.RetryAfter.Seconds()))))
		}
	case errors.Is(err, context.DeadlineExceeded): // names the cause without secrets
		msg = fmt.Sprintf("provider %s: upstream timed out (PLANPORT_TIMEOUT), request %s", pid, reqID)
	case !isUp:
		msg = fmt.Sprintf("provider %s: server or upstream connection error, see server log, request %s", pid, reqID)
	case up.Status == http.StatusUnauthorized || up.Status == http.StatusForbidden:
		msg = fmt.Sprintf("provider %s: upstream rejected credentials (HTTP %d), check the credential; details in server log, request %s", pid, up.Status, reqID)
	}
	writeErr(w, status, msg, typ)
}

// OpenAI response shapes.

type deltaBody struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chunkChoice struct {
	Index        int       `json:"index"`
	Delta        deltaBody `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

type messageOut struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type choiceOut struct {
	Index        int        `json:"index"`
	Message      messageOut `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type completion struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []choiceOut    `json:"choices"`
	Usage   provider.Usage `json:"usage"`
}
