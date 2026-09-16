package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

// Endpoint is the ChatGPT-subscription inference endpoint
// (codex-rs/model-provider-info/src/lib.rs CHATGPT_CODEX_BASE_URL plus the
// "/responses" path from codex-api/src/endpoint/responses.rs).
const Endpoint = "https://chatgpt.com/backend-api/codex/responses"

// sessionHeader is the header Claude Code puts its session id in. Reusing it
// for the backend's session-id gives prompt-cache affinity across the turns of
// one conversation, which is what the Codex CLI achieves with its own id.
const sessionHeader = "X-Claude-Code-Session-Id"

// Client performs inference against the Codex endpoint.
type Client struct {
	store    *Store
	endpoint string
	version  string
	opt      Options
	stream   StreamOptions
	headers  map[string]string
	http     *http.Client
}

// NewClient builds a Client around a credential store.
func NewClient(store *Store, endpoint, version string, opt Options, stream StreamOptions, extraHeaders map[string]string) *Client {
	if endpoint == "" {
		endpoint = Endpoint
	}
	if version == "" {
		version = DefaultClientVersion
	}
	return &Client{
		store:    store,
		endpoint: endpoint,
		version:  version,
		opt:      opt,
		stream:   stream,
		headers:  extraHeaders,
		http: &http.Client{
			// No overall timeout: a reasoning turn can legitimately stream for
			// minutes. The phases that must not hang are bounded below.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConnsPerHost:   4,
			},
		},
	}
}

// Messages handles one POST /v1/messages for a Codex model.
func (c *Client) Messages(w http.ResponseWriter, r *http.Request, req *anthropic.MessagesRequest, upstreamModel, displayModel string) {
	body, err := TranslateRequest(req, upstreamModel, c.opt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if sess := r.Header.Get(sessionHeader); sess != "" {
		body.PromptCacheKey = sess
	}

	payload, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error",
			fmt.Sprintf("encode upstream request: %v", err))
		return
	}

	token, err := c.store.AccessToken(r.Context(), c.http)
	if err != nil {
		// A dead grant is the user's to fix, and saying so beats a bare 401.
		writeError(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	c.setHeaders(upReq, r, token)

	resp, err := c.http.Do(upReq)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client hung up; nothing useful to say
		}
		writeError(w, http.StatusBadGateway, "api_error",
			fmt.Sprintf("gateway could not reach the Codex endpoint: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.relayError(w, resp)
		return
	}

	sw := anthropic.NewStreamWriter(w)
	tr := NewStreamTranslator(sw, displayModel, c.stream)
	_ = tr.Run(resp.Body)
}

// setHeaders applies the header set the Codex CLI sends. Several are not
// optional in practice, so they are set explicitly rather than left to defaults.
func (c *Client) setHeaders(up *http.Request, in *http.Request, token string) {
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", "text/event-stream")
	up.Header.Set("Authorization", "Bearer "+token)
	if account := c.store.AccountID(); account != "" {
		up.Header.Set("ChatGPT-Account-ID", account)
	}
	up.Header.Set("originator", Originator)
	up.Header.Set("User-Agent", UserAgent(c.version))
	// A literal header named "version", from the provider's static header set.
	// The backend gates model availability on it.
	up.Header.Set("version", c.version)
	// This one is not confirmed against the Codex CLI's own header set, which
	// may apply it in a shared HTTP layer. It is accepted in practice, and a
	// 400 mentioning betas is the signal to revisit it.
	up.Header.Set("OpenAI-Beta", "responses=experimental")

	if sess := in.Header.Get(sessionHeader); sess != "" {
		up.Header.Set("session-id", sess)
		up.Header.Set("thread-id", sess)
		up.Header.Set("x-client-request-id", sess)
	}
	for k, v := range c.headers {
		up.Header.Set(k, v)
	}
}

// relayError converts an upstream failure into the Anthropic error envelope,
// keeping the status so Claude Code's retry logic sees the truth.
func (c *Client) relayError(w http.ResponseWriter, resp *http.Response) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	kind := anthropicErrorKind(resp.StatusCode)
	msg := strings.TrimSpace(string(raw))
	var probe struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		switch {
		case probe.Error.Message != "":
			msg = probe.Error.Message
			if probe.Error.Code != "" {
				msg = probe.Error.Code + ": " + msg
			}
		case probe.Detail != "":
			msg = probe.Detail
		}
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		msg += " (the ChatGPT session may have expired - run 'ccgw codex login')"
	}

	if v := clampRetryAfter(resp.Header.Get("retry-after")); v != "" {
		w.Header().Set("retry-after", v)
	}
	writeError(w, resp.StatusCode, kind, "codex: "+msg)
}

// MaxRetryAfterSeconds is the largest retry-after this gateway forwards.
//
// Claude Code treats the header as a floor on its backoff and aborts the turn
// if the resulting delay would exceed 60 seconds, so a provider's larger value
// must not pass through verbatim.
const MaxRetryAfterSeconds = 60

func clampRetryAfter(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil || secs < 0 {
		return "" // an HTTP-date or junk; Claude Code parseInts this field
	}
	if secs > MaxRetryAfterSeconds {
		secs = MaxRetryAfterSeconds
	}
	return fmt.Sprint(secs)
}

// anthropicErrorKind maps an HTTP status onto Anthropic's error vocabulary,
// which is the field Claude Code classifies failures by.
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
	return "api_error"
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(anthropic.NewAPIError(kind, msg))
}
