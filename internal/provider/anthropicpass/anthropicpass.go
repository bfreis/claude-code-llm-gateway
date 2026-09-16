// Package anthropicpass proxies requests to the Anthropic API byte-for-byte.
//
// Nothing about the request body is parsed or rewritten, so every field Claude
// Code sends — betas, cache_control, thinking blocks, fields added after this
// code was written — reaches Anthropic exactly as it would have without the
// gateway in the path. That is what keeps a Claude subscription usable: the
// credential Claude Code attached is relayed untouched.
package anthropicpass

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
)

// Proxy forwards to an Anthropic-compatible endpoint.
type Proxy struct {
	target *url.URL
	auth   config.AnthropicConfig
	rp     *httputil.ReverseProxy
}

// New builds a Proxy from the anthropic section of the config.
func New(cfg config.AnthropicConfig, errLog func(*http.Request, error)) (*Proxy, error) {
	target, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("anthropic.base_url: %w", err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("anthropic.base_url %q: need scheme and host", cfg.BaseURL)
	}

	p := &Proxy{target: target, auth: cfg}
	p.rp = &httputil.ReverseProxy{
		Rewrite: p.rewrite,
		// -1 forwards each write immediately. Anything else would buffer the
		// SSE stream and trip Claude Code's first-byte watchdog.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errLog != nil {
				errLog(r, err)
			}
			WriteError(w, http.StatusBadGateway, "api_error",
				fmt.Sprintf("gateway could not reach %s: %v", target.Host, err))
		},
	}
	return p, nil
}

func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	r.SetURL(p.target)
	// SetURL keeps the inbound Host; Anthropic needs its own.
	r.Out.Host = p.target.Host
	// Claude Code sets this for a browser-origin check that does not apply to
	// a server-side hop, but it is harmless and cheaper to keep than to reason
	// about. Everything else is left exactly as it arrived.

	switch p.auth.Auth {
	case config.AuthBearer:
		if tok := os.Getenv(p.auth.TokenEnv); tok != "" {
			r.Out.Header.Del("x-api-key")
			r.Out.Header.Set("Authorization", "Bearer "+tok)
		}
	case config.AuthAPIKey:
		if key := os.Getenv(p.auth.APIKeyEnv); key != "" {
			r.Out.Header.Del("Authorization")
			r.Out.Header.Set("x-api-key", key)
		}
	case config.AuthPassthrough:
		// Leave whatever Claude Code sent.
	}

	mergeBetas(r.Out.Header, p.auth.AddBetas)

	// A subscription OAuth token is only accepted alongside this beta, and
	// gateway mode does not send it. Without this, forwarding a `claude
	// setup-token` credential through gateway mode fails at Anthropic.
	if isOAuthBearer(r.Out.Header.Get("Authorization")) {
		mergeBetas(r.Out.Header, []string{config.OAuthBeta})
	}
}

// isOAuthBearer reports whether the Authorization header carries an Anthropic
// subscription OAuth token rather than an API key.
func isOAuthBearer(auth string) bool {
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(tok), "sk-ant-oat")
}

// mergeBetas adds values to the anthropic-beta header, preserving the order
// already present and never duplicating an entry.
func mergeBetas(h http.Header, add []string) {
	if len(add) == 0 {
		return
	}
	existing := splitBetas(h.Get("anthropic-beta"))
	have := make(map[string]bool, len(existing))
	for _, v := range existing {
		have[v] = true
	}
	for _, v := range add {
		v = strings.TrimSpace(v)
		if v == "" || have[v] {
			continue
		}
		have[v] = true
		existing = append(existing, v)
	}
	if len(existing) > 0 {
		h.Set("anthropic-beta", strings.Join(existing, ","))
	}
}

func splitBetas(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ServeHTTP proxies one request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.rp.ServeHTTP(w, r)
}

// WriteError emits an Anthropic-shaped error envelope.
//
// Claude Code classifies failures by this JSON shape, so a gateway error that
// does not match it surfaces as an opaque "unknown" error in the UI.
func WriteError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"error","error":{"type":%s,"message":%s}}`,
		jsonString(kind), jsonString(msg))
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
