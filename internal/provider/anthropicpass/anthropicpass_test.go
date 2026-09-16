package anthropicpass

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
)

func proxyTo(t *testing.T, cfg config.AnthropicConfig, upstream *httptest.Server) *Proxy {
	t.Helper()
	cfg.BaseURL = upstream.URL
	if cfg.Auth == "" {
		cfg.Auth = config.AuthPassthrough
	}
	p, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// echoUpstream records the headers it receives.
func echoUpstream(got *http.Header) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Clone()
		fmt.Fprint(w, `{}`)
	}))
}

func do(p *Proxy, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestPassthroughKeepsCredential(t *testing.T) {
	var got http.Header
	up := echoUpstream(&got)
	defer up.Close()
	p := proxyTo(t, config.AnthropicConfig{Auth: config.AuthPassthrough}, up)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-example")
	do(p, req)

	if got.Get("Authorization") != "Bearer sk-ant-oat01-example" {
		t.Errorf("Authorization = %q, want it relayed untouched", got.Get("Authorization"))
	}
}

func TestBearerAuthReplacesCredential(t *testing.T) {
	t.Setenv("CCGW_TEST_TOKEN", "sk-ant-oat01-gateway")
	var got http.Header
	up := echoUpstream(&got)
	defer up.Close()
	p := proxyTo(t, config.AnthropicConfig{Auth: config.AuthBearer, TokenEnv: "CCGW_TEST_TOKEN"}, up)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-api-key", "should-be-removed")
	do(p, req)

	if got.Get("Authorization") != "Bearer sk-ant-oat01-gateway" {
		t.Errorf("Authorization = %q", got.Get("Authorization"))
	}
	if got.Get("x-api-key") != "" {
		t.Errorf("x-api-key = %q, want it cleared so the two do not conflict", got.Get("x-api-key"))
	}
}

func TestAPIKeyAuthReplacesCredential(t *testing.T) {
	t.Setenv("CCGW_TEST_APIKEY", "sk-ant-api03-gateway")
	var got http.Header
	up := echoUpstream(&got)
	defer up.Close()
	p := proxyTo(t, config.AnthropicConfig{Auth: config.AuthAPIKey, APIKeyEnv: "CCGW_TEST_APIKEY"}, up)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer should-be-removed")
	do(p, req)

	if got.Get("x-api-key") != "sk-ant-api03-gateway" {
		t.Errorf("x-api-key = %q", got.Get("x-api-key"))
	}
	if got.Get("Authorization") != "" {
		t.Errorf("Authorization = %q, want it cleared", got.Get("Authorization"))
	}
}

func TestOAuthBetaAddedForSubscriptionToken(t *testing.T) {
	var got http.Header
	up := echoUpstream(&got)
	defer up.Close()
	p := proxyTo(t, config.AnthropicConfig{Auth: config.AuthPassthrough}, up)

	// Gateway mode strips oauth-2025-04-20, which Anthropic requires for a
	// subscription token.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-example")
	req.Header.Set("anthropic-beta", "claude-code-20250219,effort-2025-11-24")
	do(p, req)

	betas := got.Get("anthropic-beta")
	if !strings.Contains(betas, config.OAuthBeta) {
		t.Errorf("anthropic-beta = %q, want it to include %s", betas, config.OAuthBeta)
	}
	if !strings.Contains(betas, "claude-code-20250219") {
		t.Errorf("anthropic-beta = %q, want the original values kept", betas)
	}
}

func TestOAuthBetaNotAddedForAPIKey(t *testing.T) {
	var got http.Header
	up := echoUpstream(&got)
	defer up.Close()
	p := proxyTo(t, config.AnthropicConfig{Auth: config.AuthPassthrough}, up)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-api-key", "sk-ant-api03-example")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	do(p, req)

	if strings.Contains(got.Get("anthropic-beta"), config.OAuthBeta) {
		t.Errorf("anthropic-beta = %q, want no oauth beta for an API key", got.Get("anthropic-beta"))
	}
}

func TestAddBetasMergeWithoutDuplicates(t *testing.T) {
	var got http.Header
	up := echoUpstream(&got)
	defer up.Close()
	p := proxyTo(t, config.AnthropicConfig{
		Auth:     config.AuthPassthrough,
		AddBetas: []string{"context-management-2025-06-27", "claude-code-20250219"},
	}, up)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("anthropic-beta", "claude-code-20250219,effort-2025-11-24")
	do(p, req)

	betas := got.Get("anthropic-beta")
	if strings.Count(betas, "claude-code-20250219") != 1 {
		t.Errorf("anthropic-beta = %q, want no duplicate", betas)
	}
	if !strings.Contains(betas, "context-management-2025-06-27") {
		t.Errorf("anthropic-beta = %q, want the added beta", betas)
	}
	// The values Claude Code chose must not be reordered away.
	if !strings.HasPrefix(betas, "claude-code-20250219,effort-2025-11-24") {
		t.Errorf("anthropic-beta = %q, want the original order preserved first", betas)
	}
}

func TestAddBetasOnEmptyHeader(t *testing.T) {
	var got http.Header
	up := echoUpstream(&got)
	defer up.Close()
	p := proxyTo(t, config.AnthropicConfig{
		Auth:     config.AuthPassthrough,
		AddBetas: []string{"context-management-2025-06-27"},
	}, up)

	do(p, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if got.Get("anthropic-beta") != "context-management-2025-06-27" {
		t.Errorf("anthropic-beta = %q", got.Get("anthropic-beta"))
	}
}

func TestUpstreamFailureUsesAnthropicErrorEnvelope(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	up.Close() // nothing is listening, so the proxy hits a dial error

	p := proxyTo(t, config.AnthropicConfig{Auth: config.AuthPassthrough}, up)
	rec := do(p, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"type":"error"`) {
		t.Errorf("body = %s, want an Anthropic error envelope", body)
	}
}

func TestJSONStringEscaping(t *testing.T) {
	got := jsonString(`he said "hi"` + "\n\tdone\\")
	want := `"he said \"hi\"\n\tdone\\"`
	if got != want {
		t.Errorf("jsonString = %s, want %s", got, want)
	}
}
