// Package server wires the HTTP surface Claude Code talks to.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
	"github.com/bfreis/claude-code-llm-gateway/internal/config"
	"github.com/bfreis/claude-code-llm-gateway/internal/provider/anthropiccompat"
	"github.com/bfreis/claude-code-llm-gateway/internal/provider/anthropicpass"
	"github.com/bfreis/claude-code-llm-gateway/internal/provider/codex"
	"github.com/bfreis/claude-code-llm-gateway/internal/provider/openai"
	"github.com/bfreis/claude-code-llm-gateway/internal/router"
)

// maxRequestBody bounds how much of a request the gateway buffers before
// routing. Claude Code sends large contexts; this is generous but finite.
const maxRequestBody = 256 << 20 // 256 MiB

// Server is the gateway's HTTP handler.
type Server struct {
	cfg     *config.Config
	router  *router.Router
	passthr *anthropicpass.Proxy
	clients map[string]backend
	log     *slog.Logger
}

// backend is one configured non-Anthropic provider. The two implementations
// differ in where translation happens: openai does it here, anthropic-compatible
// leaves it to the endpoint.
type backend interface {
	// Messages handles one POST /v1/messages. raw is the body exactly as Claude
	// Code sent it; req is the same body decoded, for backends that need it.
	Messages(w http.ResponseWriter, r *http.Request, raw []byte,
		req *anthropic.MessagesRequest, upstreamModel, displayModel string)
}

// openAIBackend adapts openai.Client, which works from the decoded request.
type openAIBackend struct{ c *openai.Client }

func (b openAIBackend) Messages(w http.ResponseWriter, r *http.Request, _ []byte,
	req *anthropic.MessagesRequest, upstreamModel, displayModel string) {
	b.c.Messages(w, r, req, upstreamModel, displayModel)
}

// codexBackend adapts codex.Client, which works from the decoded request.
type codexBackend struct{ c *codex.Client }

func (b codexBackend) Messages(w http.ResponseWriter, r *http.Request, _ []byte,
	req *anthropic.MessagesRequest, upstreamModel, displayModel string) {
	b.c.Messages(w, r, req, upstreamModel, displayModel)
}

// compatBackend adapts anthropiccompat.Client, which works from the raw body.
type compatBackend struct{ c *anthropiccompat.Client }

func (b compatBackend) Messages(w http.ResponseWriter, r *http.Request, raw []byte,
	_ *anthropic.MessagesRequest, upstreamModel, _ string) {
	b.c.Messages(w, r, raw, upstreamModel)
}

// CountTokens lets the backend answer instead of the gateway estimating.
func (b compatBackend) CountTokens(w http.ResponseWriter, r *http.Request, raw []byte, upstreamModel string) {
	b.c.CountTokens(w, r, raw, upstreamModel)
}

// New builds a Server from validated config.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	pass, err := anthropicpass.New(cfg.Anthropic, func(r *http.Request, err error) {
		log.Error("anthropic passthrough failed", "path", r.URL.Path, "err", err)
	})
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:     cfg,
		router:  router.New(cfg),
		passthr: pass,
		clients: map[string]backend{},
		log:     log,
	}
	for _, p := range cfg.Providers {
		switch p.Type {
		case config.TypeCodex:
			client, err := newCodexBackend(p)
			if err != nil {
				return nil, err
			}
			s.clients[p.Name] = client
		case config.TypeAnthropicCompatible:
			client, err := anthropiccompat.New(p,
				anthropiccompat.Options{DropPlanTools: !p.KeepPlanTools},
				func(r *http.Request, err error) {
					log.Error("provider forward failed", "provider", p.Name, "err", err)
				})
			if err != nil {
				return nil, err
			}
			s.clients[p.Name] = compatBackend{c: client}
		default:
			s.clients[p.Name] = openAIBackend{c: openai.NewClient(p, optionsFor(p))}
		}
	}
	return s, nil
}

// newCodexBackend loads the ChatGPT credential and builds the client.
func newCodexBackend(p config.Provider) (backend, error) {
	path := p.AuthPath
	if path == "" {
		resolved, err := codex.DefaultAuthPath()
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p.Name, err)
		}
		path = resolved
	}
	store, err := codex.NewStore(path)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w - sign in with 'ccgw codex login'", p.Name, err)
	}

	reasoning := codex.ReasoningAsThinking
	if p.Reasoning == config.ReasoningDrop {
		reasoning = codex.ReasoningDrop
	}
	client := codex.NewClient(store, p.BaseURL, p.ClientVersion,
		codex.Options{
			DropPlanTools: !p.KeepPlanTools,
			ServiceTier:   p.ServiceTier,
		},
		codex.StreamOptions{ReasoningMode: reasoning},
		p.Headers,
	)
	return codexBackend{c: client}, nil
}

// optionsFor maps a provider's configuration onto the translator's knobs.
func optionsFor(p config.Provider) openai.Options {
	reasoning := openai.ReasoningAsThinking
	if p.Reasoning == config.ReasoningDrop {
		reasoning = openai.ReasoningDrop
	}
	return openai.Options{
		UseMaxCompletionTokens: p.MaxTokensField == config.MaxCompletionTokensField,
		DropTemperature:        p.DropTemperature,
		MaxTokensCap:           p.MaxTokensCap,
		ReasoningMode:          reasoning,
		DropPlanTools:          !p.KeepPlanTools,
	}
}

// Handler returns the configured mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Claude Code probes this before it will use a base URL at all. Observed
	// on the wire as `HEAD /api/hello`.
	mux.HandleFunc("/api/hello", s.handleHello)

	// Gateway model discovery reads this when CLAUDE_CODE_USE_GATEWAY and
	// CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY are both set.
	mux.HandleFunc("GET /v1/models", s.handleModels)

	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)

	// Anything else Claude Code asks for goes to Anthropic untouched, so an
	// endpoint this gateway has never heard of still works.
	mux.Handle("/", s.passthr)

	return s.withLogging(mux)
}

func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		fmt.Fprint(w, `{"ok":true}`)
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	list := anthropic.ModelList{Data: []anthropic.ModelInfo{}}
	for _, m := range s.router.Catalogue() {
		list.Data = append(list.Data, anthropic.ModelInfo{
			Type:        "model",
			ID:          m.ID,
			DisplayName: m.DisplayName,
			Description: m.Description,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("read request body: %v", err))
		return
	}

	model, err := peekModel(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	route := s.router.Resolve(model)
	if route.Kind == router.KindAnthropic {
		// A conversation that used a provider model earlier carries thinking
		// blocks this gateway synthesised, which have no Anthropic signature.
		// Anthropic 400s on those, and Claude Code recovers by discarding every
		// thinking block in the conversation — its own signed ones included.
		// Removing just the unsigned blocks avoids both. Bodies without them
		// are forwarded untouched.
		body, stripped := anthropic.StripUnsignedThinking(raw)
		if stripped > 0 {
			s.log.Debug("removed unsigned thinking blocks before Anthropic",
				"count", stripped, "model", model)
		}
		s.log.Debug("routing to anthropic", "model", model)
		replayBody(r, body)
		s.passthr.ServeHTTP(w, r)
		return
	}

	client, ok := s.clients[route.Provider.Name]
	if !ok {
		writeError(w, http.StatusInternalServerError, "api_error",
			fmt.Sprintf("no client for provider %q", route.Provider.Name))
		return
	}

	var req anthropic.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("decode messages request: %v", err))
		return
	}
	if route.Model.MaxTokens > 0 && (req.MaxTokens == 0 || req.MaxTokens > route.Model.MaxTokens) {
		req.MaxTokens = route.Model.MaxTokens
	}

	s.log.Debug("routing to provider",
		"provider", route.Provider.Name, "model", route.UpstreamModel, "stream", req.Stream)
	// The model echoed back to Claude Code is the ID it selected, so the UI
	// keeps showing the picker's name rather than the upstream one.
	client.Messages(w, r, raw, &req, route.UpstreamModel, model)
}

// tokenCounter is a backend that can answer count_tokens itself.
type tokenCounter interface {
	CountTokens(w http.ResponseWriter, r *http.Request, raw []byte, upstreamModel string)
}

// handleCountTokens answers the token-counting endpoint.
//
// Claude models go to Anthropic and an Anthropic-compatible backend answers for
// itself; both know better than this gateway does. An OpenAI-translated model
// has no equivalent endpoint, so it gets an estimate rather than an error: a
// wrong count degrades a progress indicator, a failure breaks the turn.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model, err := peekModel(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	route := s.router.Resolve(model)
	if route.Kind == router.KindAnthropic {
		replayBody(r, raw)
		s.passthr.ServeHTTP(w, r)
		return
	}
	if counter, ok := s.clients[route.Provider.Name].(tokenCounter); ok {
		counter.CountTokens(w, r, raw, route.UpstreamModel)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimateTokens(raw)})
}

// estimateTokens approximates the prompt size of a Messages request.
//
// Claude Code calls count_tokens in three places, and all of them fall back to
// their own local estimate if the call fails, so an approximation here is safe:
// deciding whether an oversized MCP tool result needs truncating, sizing a
// skill, and populating /context (which issues many calls per turn). Counting
// the text content rather than the raw JSON keeps structural overhead and
// escaping out of the figure.
func estimateTokens(raw []byte) int {
	chars := textBytes(raw)
	if chars == 0 {
		chars = len(raw) // undecodable body: fall back to the whole payload
	}
	// Roughly four characters per token for English prose.
	if n := chars / 4; n > 0 {
		return n
	}
	return 1
}

// textBytes sums the natural-language content of a Messages request.
func textBytes(raw []byte) int {
	var req anthropic.MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0
	}
	total := blockBytes(req.System)
	for _, m := range req.Messages {
		total += blockBytes(m.Content)
	}
	for _, t := range req.Tools {
		total += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return total
}

// blockBytes counts the text in a string-or-blocks content field.
func blockBytes(raw json.RawMessage) int {
	blocks, err := anthropic.DecodeContent(raw)
	if err != nil {
		return len(raw)
	}
	var total int
	for _, b := range blocks {
		switch b.Type {
		case anthropic.BlockText:
			total += len(b.Text)
		case anthropic.BlockThinking:
			total += len(b.Thinking)
		case anthropic.BlockToolUse:
			total += len(b.Name) + len(b.Input)
		case anthropic.BlockToolResult:
			total += blockBytes(b.Content)
		case anthropic.BlockImage, anthropic.BlockDocument:
			// Base64 payloads are not text and would swamp the estimate; they
			// are billed by dimensions, which the gateway cannot see.
			if b.Source != nil {
				total += len(b.Source.URL)
			}
		}
	}
	return total
}

// peekModel pulls just the model field out of a request body.
func peekModel(raw []byte) (string, error) {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", fmt.Errorf("request body is not valid json: %w", err)
	}
	if probe.Model == "" {
		return "", fmt.Errorf("request is missing the model field")
	}
	return probe.Model, nil
}

// replayBody puts an already-read body back on the request so it can be
// proxied.
func replayBody(r *http.Request, raw []byte) {
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.Header.Del("Content-Length")
	r.Header.Set("Content-Length", fmt.Sprint(len(raw)))
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(anthropic.NewAPIError(kind, msg))
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		s.log.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

// statusRecorder captures the status code while preserving Flusher, which the
// SSE path depends on.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets ReverseProxy reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
