// Package anthropiccompat forwards an Anthropic Messages request to a backend
// that already speaks that protocol, instead of translating it.
//
// The motivating case is a backend whose OpenAI-compatible route cannot carry
// tool calls while its Anthropic Messages route can. Claude Code is entirely
// tool-driven, so a route without tool support is no use to it; the request is
// better sent in the shape it already has and translated by the far end.
//
// Only two things are changed on the way through: the model ID is rewritten to
// the backend's own, and Claude Code's plan-mode tools are withheld by default.
// Every other field is forwarded as it arrived.
package anthropiccompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
	"github.com/bfreis/claude-code-llm-gateway/internal/config"
)

// inboundCredentials are the caller's Anthropic credentials. They are removed
// before the request leaves for the backend: a proxy that serves some other
// provider has no business receiving a Claude subscription token, and it would
// ignore it anyway.
var inboundCredentials = []string{
	"authorization",
	"x-api-key",
	"proxy-authorization",
	"cookie",
}

// Options tune what the forwarder rewrites.
type Options struct {
	// DropPlanTools withholds Claude Code's plan-mode tools.
	DropPlanTools bool
}

// Client forwards to one Anthropic-compatible backend.
type Client struct {
	cfg    config.Provider
	opt    Options
	target *url.URL
	rp     *httputil.ReverseProxy
}

// New builds a Client for a configured provider.
func New(p config.Provider, opt Options, errLog func(*http.Request, error)) (*Client, error) {
	target, err := url.Parse(strings.TrimRight(p.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("provider %q: base_url: %w", p.Name, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("provider %q: base_url %q: need scheme and host", p.Name, p.BaseURL)
	}

	c := &Client{cfg: p, opt: opt, target: target}
	c.rp = &httputil.ReverseProxy{
		Rewrite: c.rewrite,
		// -1 forwards each write as it arrives. Anything else would buffer the
		// SSE stream.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errLog != nil {
				errLog(r, err)
			}
			writeError(w, http.StatusBadGateway, "api_error",
				fmt.Sprintf("gateway could not reach provider %q at %s: %v", p.Name, target.Host, err))
		},
	}
	return c, nil
}

func (c *Client) rewrite(r *httputil.ProxyRequest) {
	r.SetURL(c.target)
	r.Out.Host = c.target.Host

	for _, h := range inboundCredentials {
		r.Out.Header.Del(h)
	}
	if key := c.cfg.Key(); key != "" {
		r.Out.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range c.cfg.Headers {
		r.Out.Header.Set(k, v)
	}
}

// Messages forwards one POST /v1/messages to the backend. raw is the body as
// Claude Code sent it, and upstreamModel is the backend's own model ID.
func (c *Client) Messages(w http.ResponseWriter, r *http.Request, raw []byte, upstreamModel string) {
	c.forward(w, r, raw, upstreamModel)
}

// CountTokens forwards POST /v1/messages/count_tokens.
//
// An Anthropic-compatible backend answers this itself, with a real tokenizer,
// which beats the gateway guessing from the request size.
func (c *Client) CountTokens(w http.ResponseWriter, r *http.Request, raw []byte, upstreamModel string) {
	c.forward(w, r, raw, upstreamModel)
}

// forward rewrites the body and proxies the request to the backend, preserving
// the inbound path so one method serves every Messages-API route.
func (c *Client) forward(w http.ResponseWriter, r *http.Request, raw []byte, upstreamModel string) {
	body, err := rewriteBody(raw, upstreamModel, c.opt.DropPlanTools)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))
	r.Body = newBody(body)

	c.rp.ServeHTTP(w, r)
}

func newBody(b []byte) *bodyReader { return &bodyReader{Reader: bytes.NewReader(b)} }

type bodyReader struct{ *bytes.Reader }

func (b *bodyReader) Close() error { return nil }

// rewriteBody swaps in the backend's model ID and, optionally, removes the
// plan-mode tools. Unknown fields are preserved: the body is decoded one level
// deep and re-encoded, so anything this gateway does not understand survives.
func rewriteBody(raw []byte, model string, dropPlanTools bool) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("request body is not valid json: %w", err)
	}

	encodedModel, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	req["model"] = encodedModel

	if dropPlanTools {
		if tools, ok := req["tools"]; ok {
			filtered, changed, err := filterPlanTools(tools)
			if err == nil && changed {
				req["tools"] = filtered
			}
		}
	}
	return json.Marshal(req)
}

// filterPlanTools removes EnterPlanMode and ExitPlanMode from a tool list.
func filterPlanTools(raw json.RawMessage) (json.RawMessage, bool, error) {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw, false, err
	}
	kept := make([]map[string]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var name string
		if rawName, ok := t["name"]; ok {
			_ = json.Unmarshal(rawName, &name)
		}
		if PlanTools[name] {
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == len(tools) {
		return raw, false, nil
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return raw, false, err
	}
	return out, true, nil
}

// PlanTools are Claude Code's plan-mode tools. They drive its own plan/accept
// workflow rather than doing work, and a non-Claude model handed them calls
// them unprompted.
var PlanTools = map[string]bool{
	"EnterPlanMode": true,
	"ExitPlanMode":  true,
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(anthropic.NewAPIError(kind, msg))
}
