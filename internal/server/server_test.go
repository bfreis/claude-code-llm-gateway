package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
)

// capture records what a fake upstream received.
type capture struct {
	path   string
	body   []byte
	header http.Header
}

func newTestServer(t *testing.T, yaml string) http.Handler {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	s, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return s.Handler()
}

func TestHelloProbe(t *testing.T) {
	h := newTestServer(t, "{}\n")
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/hello", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s /api/hello = %d, want 200", method, rec.Code)
		}
	}
}

func TestModelsCatalogueUsesPrefixedIDs(t *testing.T) {
	h := newTestServer(t, `
providers:
  - name: openai
models:
  - id: gpt-5.6
    provider: openai
    display_name: GPT-5.6
    description: fast
`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?limit=1000", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("data = %+v", got.Data)
	}
	// Claude Code's discovery drops ids without "claude"/"anthropic", so the
	// advertised id must carry the prefix.
	if got.Data[0].ID != "anthropic/gpt-5.6" {
		t.Errorf("id = %q, want anthropic/gpt-5.6", got.Data[0].ID)
	}
	if got.Data[0].DisplayName != "GPT-5.6" || got.Data[0].Description != "fast" {
		t.Errorf("entry = %+v", got.Data[0])
	}
}

func TestClaudeModelIsProxiedByteForByte(t *testing.T) {
	var got capture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.RequestURI()
		got.body, _ = io.ReadAll(r.Body)
		got.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message","content":[]}`)
	}))
	defer upstream.Close()

	h := newTestServer(t, "anthropic:\n  base_url: "+upstream.URL+"\n")

	// A body with fields the gateway knows nothing about: all of them must
	// survive, which is the whole point of the passthrough.
	body := `{"model":"claude-opus-5","some_future_field":{"a":1},"messages":[],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-ant-oat-example")
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got.path != "/v1/messages?beta=true" {
		t.Errorf("upstream path = %q, want the query preserved", got.path)
	}
	if string(got.body) != body {
		t.Errorf("upstream body = %s\nwant identical bytes: %s", got.body, body)
	}
	if h := got.header.Get("Authorization"); h != "Bearer sk-ant-oat-example" {
		t.Errorf("Authorization = %q, want the caller's credential relayed untouched", h)
	}
	// The betas Claude Code chose survive; the oauth beta is added because the
	// forwarded credential is a subscription token.
	if h := got.header.Get("anthropic-beta"); !strings.Contains(h, "interleaved-thinking-2025-05-14") {
		t.Errorf("anthropic-beta = %q, want the original values preserved", h)
	}
}

func TestUnknownPathFallsThroughToAnthropic(t *testing.T) {
	var got capture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	h := newTestServer(t, "anthropic:\n  base_url: "+upstream.URL+"\n")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/organizations/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got.path != "/v1/organizations/me" {
		t.Errorf("upstream path = %q, want the request forwarded", got.path)
	}
}

func TestProviderModelIsTranslated(t *testing.T) {
	var got capture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.body, _ = io.ReadAll(r.Body)
		got.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	t.Setenv("CCGW_TEST_OPENAI_KEY", "sk-test")
	h := newTestServer(t, `
providers:
  - name: openai
    base_url: `+upstream.URL+`
    api_key_env: CCGW_TEST_OPENAI_KEY
models:
  - id: gpt-5.6
    provider: openai
`)

	body := `{"model":"anthropic/gpt-5.6","max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got.path != "/chat/completions" {
		t.Errorf("upstream path = %q", got.path)
	}
	if h := got.header.Get("Authorization"); h != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want the provider key", h)
	}

	var sent map[string]any
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	// The alias prefix is a Claude Code concern only; the provider must see
	// its own model id.
	if sent["model"] != "gpt-5.6" {
		t.Errorf("upstream model = %v, want the prefix stripped", sent["model"])
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["model"] != "anthropic/gpt-5.6" {
		t.Errorf("response model = %v, want the id Claude Code picked", out["model"])
	}
	content := out["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "pong" {
		t.Errorf("content = %v", content)
	}
}

func TestProviderModelStreams(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	h := newTestServer(t, `
providers:
  - name: openai
    base_url: `+upstream.URL+`
models:
  - id: gpt-5.6
    provider: openai
`)
	body := `{"model":"anthropic/gpt-5.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start",
		"event: content_block_delta", "event: content_block_stop",
		"event: message_delta", "event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream is missing %q\n%s", want, out)
		}
	}
}

func TestProviderErrorUsesAnthropicEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"slow down","type":"rate_limit_exceeded"}}`)
	}))
	defer upstream.Close()

	h := newTestServer(t, `
providers:
  - name: openai
    base_url: `+upstream.URL+`
models:
  - id: gpt-5.6
    provider: openai
`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gpt-5.6","messages":[]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the upstream status preserved", rec.Code)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Claude Code classifies failures by this shape.
	if env.Type != "error" || env.Error.Type == "" || !strings.Contains(env.Error.Message, "slow down") {
		t.Errorf("envelope = %+v", env)
	}
}

func TestMissingModelIsRejected(t *testing.T) {
	h := newTestServer(t, "{}\n")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		bytes.NewReader([]byte(`{"messages":[]}`))))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("body = %s, want an Anthropic error envelope", rec.Body)
	}
}

func TestCountTokensEstimatesForProviderModels(t *testing.T) {
	h := newTestServer(t, `
providers:
  - name: openai
models:
  - id: gpt-5.6
    provider: openai
`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"anthropic/gpt-5.6","messages":[{"role":"user","content":"hello"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.InputTokens <= 0 {
		t.Errorf("input_tokens = %d, want a positive estimate", out.InputTokens)
	}
}

func TestEstimateTokensCountsTextNotJSONOverhead(t *testing.T) {
	// The same prose, once bare and once buried under tool-call structure.
	// A raw-length estimate would rate the second far higher; the content is
	// what Claude Code is actually asking about.
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("word ", 200) + `"}]}`)
	got := estimateTokens(plain)
	want := len(strings.Repeat("word ", 200)) / 4
	if got != want {
		t.Errorf("estimateTokens = %d, want %d (text length / 4)", got, want)
	}
}

func TestEstimateTokensIgnoresBase64Images(t *testing.T) {
	// A megabyte of base64 is not a megabyte of prose. Counting it would make
	// every screenshot look like an oversized prompt.
	img := strings.Repeat("A", 100000)
	withImage := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"look"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + img + `"}}]}]}`)
	if got := estimateTokens(withImage); got > 100 {
		t.Errorf("estimateTokens = %d, want the base64 payload excluded", got)
	}
}

func TestEstimateTokensCountsToolResults(t *testing.T) {
	// The MCP truncation path asks about a large tool result; if it did not
	// count, oversized results would never be trimmed.
	big := strings.Repeat("x", 40000)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":"` + big + `"}]}]}`)
	if got := estimateTokens(body); got < 9000 {
		t.Errorf("estimateTokens = %d, want the tool result counted (~10000)", got)
	}
}

func TestEstimateTokensFallsBackOnGarbage(t *testing.T) {
	if got := estimateTokens([]byte("not json at all")); got < 1 {
		t.Errorf("estimateTokens = %d, want a positive fallback", got)
	}
}

func TestProviderErrorTypeUsesAnthropicVocabulary(t *testing.T) {
	// Claude Code classifies failures by error.type, so the provider's own
	// name for the condition must not leak into that field.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"quota exhausted","type":"insufficient_quota"}}`)
	}))
	defer upstream.Close()

	h := newTestServer(t, `
providers:
  - name: openai
    base_url: `+upstream.URL+`
models:
  - id: gpt-5.6
    provider: openai
`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gpt-5.6","messages":[]}`)))

	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", env.Error.Type)
	}
	// The upstream wording is still useful to a human; it belongs in message.
	if !strings.Contains(env.Error.Message, "insufficient_quota") ||
		!strings.Contains(env.Error.Message, "quota exhausted") {
		t.Errorf("error.message = %q, want the upstream detail preserved", env.Error.Message)
	}
	// A 3600s retry-after would make Claude Code abort the turn instead of
	// retrying.
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want it clamped to 60", got)
	}
}
