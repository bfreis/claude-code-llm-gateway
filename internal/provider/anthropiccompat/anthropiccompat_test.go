package anthropiccompat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
)

type capture struct {
	path   string
	body   []byte
	header http.Header
}

func newClient(t *testing.T, upstream string, opt Options, p config.Provider) *Client {
	t.Helper()
	p.Name = "codex"
	p.BaseURL = upstream
	c, err := New(p, opt, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func echoUpstream(got *capture) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.body, _ = io.ReadAll(r.Body)
		got.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
}

func post(c *Client, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	if strings.HasSuffix(path, "count_tokens") {
		c.CountTokens(rec, req, []byte(body), "gpt-5.6-sol")
	} else {
		c.Messages(rec, req, []byte(body), "gpt-5.6-sol")
	}
	return rec
}

func TestModelIsRewrittenAndEverythingElseSurvives(t *testing.T) {
	var got capture
	up := echoUpstream(&got)
	defer up.Close()
	c := newClient(t, up.URL, Options{}, config.Provider{})

	body := `{"model":"anthropic/gpt-5.6-sol","stream":true,"some_future_field":{"a":1},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	post(c, "/v1/messages", body, nil)

	var sent map[string]any
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if sent["model"] != "gpt-5.6-sol" {
		t.Errorf("model = %v, want the backend's own ID", sent["model"])
	}
	if sent["stream"] != true {
		t.Errorf("stream = %v, want it preserved", sent["stream"])
	}
	// A field this gateway has never heard of must still reach the backend.
	if sent["some_future_field"] == nil {
		t.Error("unknown fields must be forwarded")
	}
}

func TestCallerCredentialsNeverReachTheBackend(t *testing.T) {
	// The backend serves some other provider's subscription; a Claude
	// credential has no business arriving there.
	var got capture
	up := echoUpstream(&got)
	defer up.Close()
	c := newClient(t, up.URL, Options{}, config.Provider{})

	post(c, "/v1/messages", `{"model":"m","messages":[]}`, map[string]string{
		"Authorization": "Bearer sk-ant-oat01-secret",
		"x-api-key":     "sk-ant-api03-secret",
		"Cookie":        "session=secret",
	})

	for _, h := range []string{"Authorization", "x-api-key", "Cookie"} {
		if v := got.header.Get(h); v != "" {
			t.Errorf("%s reached the backend: %q", h, v)
		}
	}
}

func TestProviderKeyIsSentWhenConfigured(t *testing.T) {
	t.Setenv("CCGW_TEST_PROXY_KEY", "proxy-token")
	var got capture
	up := echoUpstream(&got)
	defer up.Close()
	c := newClient(t, up.URL, Options{}, config.Provider{APIKeyEnv: "CCGW_TEST_PROXY_KEY"})

	post(c, "/v1/messages", `{"model":"m","messages":[]}`, map[string]string{
		"Authorization": "Bearer sk-ant-oat01-secret",
	})
	if got.header.Get("Authorization") != "Bearer proxy-token" {
		t.Errorf("Authorization = %q, want the provider's own key", got.header.Get("Authorization"))
	}
}

func TestPlanToolsAreWithheldByDefault(t *testing.T) {
	var got capture
	up := echoUpstream(&got)
	defer up.Close()
	c := newClient(t, up.URL, Options{DropPlanTools: true}, config.Provider{})

	body := `{"model":"m","messages":[],"tools":[` +
		`{"name":"Read","input_schema":{}},` +
		`{"name":"EnterPlanMode","input_schema":{}},` +
		`{"name":"ExitPlanMode","input_schema":{}}]}`
	post(c, "/v1/messages", body, nil)

	var sent struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Name != "Read" {
		t.Errorf("tools = %+v, want only Read", sent.Tools)
	}
}

func TestPlanToolsKeptWhenAsked(t *testing.T) {
	var got capture
	up := echoUpstream(&got)
	defer up.Close()
	c := newClient(t, up.URL, Options{DropPlanTools: false}, config.Provider{})

	post(c, "/v1/messages", `{"model":"m","messages":[],"tools":[{"name":"ExitPlanMode"}]}`, nil)
	if !strings.Contains(string(got.body), "ExitPlanMode") {
		t.Errorf("plan tool was dropped: %s", got.body)
	}
}

func TestCountTokensKeepsThePath(t *testing.T) {
	// The backend answers this itself, which beats the gateway estimating.
	var got capture
	up := echoUpstream(&got)
	defer up.Close()
	c := newClient(t, up.URL, Options{}, config.Provider{})

	post(c, "/v1/messages/count_tokens", `{"model":"anthropic/gpt-5.6-sol","messages":[]}`, nil)
	if got.path != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q, want the count_tokens route", got.path)
	}
	var sent map[string]any
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "gpt-5.6-sol" {
		t.Errorf("model = %v, want it rewritten here too", sent["model"])
	}
}

func TestStreamingIsRelayed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer up.Close()
	c := newClient(t, up.URL, Options{}, config.Provider{})

	rec := post(c, "/v1/messages", `{"model":"m","stream":true,"messages":[]}`, nil)
	body := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %q:\n%s", want, body)
		}
	}
}

func TestUnreachableBackendUsesTheAnthropicErrorEnvelope(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	up.Close() // nothing is listening
	c := newClient(t, up.URL, Options{}, config.Provider{})

	rec := post(c, "/v1/messages", `{"model":"m","messages":[]}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("body = %s, want an Anthropic error envelope", rec.Body)
	}
}

func TestInvalidBodyIsRejectedBeforeForwarding(t *testing.T) {
	var got capture
	up := echoUpstream(&got)
	defer up.Close()
	c := newClient(t, up.URL, Options{}, config.Provider{})

	rec := post(c, "/v1/messages", `not json`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if got.body != nil {
		t.Error("an unparseable body must not reach the backend")
	}
}

func TestNewRejectsABadBaseURL(t *testing.T) {
	if _, err := New(config.Provider{Name: "x", BaseURL: "not-a-url"}, Options{}, nil); err == nil {
		t.Error("New should reject a base_url with no scheme or host")
	}
}
