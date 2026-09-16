package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
	"github.com/bfreis/claude-code-llm-gateway/internal/config"
)

// Client talks to one OpenAI-compatible backend.
type Client struct {
	cfg  config.Provider
	opt  Options
	http *http.Client
}

// NewClient builds a Client for a configured provider.
func NewClient(p config.Provider, opt Options) *Client {
	return &Client{
		cfg: p,
		opt: opt,
		http: &http.Client{
			// No overall timeout: a long agentic turn can legitimately stream
			// for many minutes. The transport's own timeouts below bound the
			// phases that must not hang.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConnsPerHost:   8,
			},
		},
	}
}

// Messages handles one POST /v1/messages for this backend.
func (c *Client) Messages(w http.ResponseWriter, r *http.Request, req *anthropic.MessagesRequest, upstreamModel, displayModel string) {
	chatReq, err := TranslateRequest(req, upstreamModel, c.opt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("encode upstream request: %v", err))
		return
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", acceptFor(req.Stream))
	if key := c.cfg.Key(); key != "" {
		upReq.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range c.cfg.Headers {
		upReq.Header.Set(k, v)
	}

	resp, err := c.http.Do(upReq)
	if err != nil {
		if r.Context().Err() != nil {
			return // client hung up; nothing useful to say
		}
		writeError(w, http.StatusBadGateway, "api_error",
			fmt.Sprintf("gateway could not reach provider %q: %v", c.cfg.Name, err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.relayError(w, resp)
		return
	}

	if req.Stream {
		c.stream(w, resp, displayModel)
		return
	}
	c.complete(w, resp, displayModel)
}

func acceptFor(stream bool) string {
	if stream {
		return "text/event-stream"
	}
	return "application/json"
}

func (c *Client) stream(w http.ResponseWriter, resp *http.Response, displayModel string) {
	sw := anthropic.NewStreamWriter(w)

	// A model that reasons for minutes before its first token would otherwise
	// trip Claude Code's 300s idle abort.
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sw.Keepalive(ctx, anthropic.KeepaliveInterval)
	}()
	// Cancel first, then wait: writing to the ResponseWriter after this
	// handler returns is not allowed, so the keepalive must be finished before
	// stream() does.
	defer func() {
		stop()
		<-done
	}()

	tr := NewStreamTranslator(sw, displayModel, c.opt)
	_ = tr.Run(resp.Body)
}

func (c *Client) complete(w http.ResponseWriter, resp *http.Response, displayModel string) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("read provider response: %v", err))
		return
	}
	var chat ChatResponse
	if err := json.Unmarshal(raw, &chat); err != nil {
		writeError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("decode provider response: %v", err))
		return
	}
	out, err := TranslateResponse(&chat, displayModel, c.opt)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// MaxRetryAfterSeconds is the largest retry-after the gateway will forward.
//
// Claude Code treats retry-after as a floor on its backoff and aborts the turn
// outright ("api_request_retry_after_too_long") if the resulting delay exceeds
// 60 seconds. Providers routinely send much larger values on a quota error, so
// passing one through verbatim turns a retryable failure into a dead turn.
const MaxRetryAfterSeconds = 60

// relayError converts an upstream failure into the Anthropic error envelope,
// preserving the status code so Claude Code's retry logic sees the truth.
func (c *Client) relayError(w http.ResponseWriter, resp *http.Response) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	// The error *type* must stay in Anthropic's vocabulary: Claude Code
	// classifies failures by it, and a provider's own name for the condition
	// ("rate_limit_exceeded", "insufficient_quota") means nothing to it. The
	// upstream wording goes in the message, where it is useful to a human.
	kind := anthropicErrorKind(resp.StatusCode)
	msg := strings.TrimSpace(string(raw))
	var oe ErrorResponse
	if err := json.Unmarshal(raw, &oe); err == nil && oe.Error.Message != "" {
		msg = oe.Error.Message
		if oe.Error.Type != "" {
			msg = oe.Error.Type + ": " + msg
		}
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}

	if v := clampRetryAfter(resp.Header.Get("retry-after")); v != "" {
		w.Header().Set("retry-after", v)
	}
	for _, h := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	writeError(w, resp.StatusCode, kind, fmt.Sprintf("provider %q: %s", c.cfg.Name, msg))
}

// clampRetryAfter caps a retry-after header at MaxRetryAfterSeconds.
//
// Only the integer-seconds form is forwarded: Claude Code parses the header
// with parseInt, so an HTTP-date value would read as NaN and be ignored anyway.
func clampRetryAfter(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return "" // an HTTP-date, or junk: drop it rather than confuse the client
	}
	if secs > MaxRetryAfterSeconds {
		secs = MaxRetryAfterSeconds
	}
	return strconv.Itoa(secs)
}

// anthropicErrorKind maps an HTTP status onto Anthropic's error type names.
func anthropicErrorKind(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "api_error"
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(anthropic.NewAPIError(kind, msg))
}
