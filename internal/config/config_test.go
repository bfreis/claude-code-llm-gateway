package config

import "testing"

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte("{}\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Listen != DefaultListen {
		t.Errorf("Listen = %q, want %q", c.Listen, DefaultListen)
	}
	if c.Prefix() != DefaultAliasPrefix {
		t.Errorf("Prefix() = %q, want %q", c.Prefix(), DefaultAliasPrefix)
	}
	if c.Anthropic.Auth != AuthPassthrough {
		t.Errorf("Anthropic.Auth = %q, want %q", c.Anthropic.Auth, AuthPassthrough)
	}
	if c.Anthropic.BaseURL != DefaultAnthropicBaseURL {
		t.Errorf("Anthropic.BaseURL = %q, want %q", c.Anthropic.BaseURL, DefaultAnthropicBaseURL)
	}
}

func TestParseEmptyPrefixIsHonoured(t *testing.T) {
	c, err := Parse([]byte("alias_prefix: \"\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Prefix() != "" {
		t.Errorf("Prefix() = %q, want empty", c.Prefix())
	}
}

func TestParseProviderDefaults(t *testing.T) {
	c, err := Parse([]byte("providers:\n  - name: openai\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := c.Providers[0]
	if p.Type != "openai" {
		t.Errorf("Type = %q, want openai", p.Type)
	}
	if p.BaseURL != DefaultOpenAIBaseURL {
		t.Errorf("BaseURL = %q, want %q", p.BaseURL, DefaultOpenAIBaseURL)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown field", "listne: x\n", "parse config"},
		{"unknown auth", "anthropic:\n  auth: magic\n", "unknown mode"},
		{"bearer without env", "anthropic:\n  auth: bearer\n", "requires anthropic.token_env"},
		{"api_key without env", "anthropic:\n  auth: api_key\n", "requires anthropic.api_key_env"},
		{"provider without name", "providers:\n  - type: openai\n", "name is required"},
		{"duplicate provider", "providers:\n  - name: a\n  - name: a\n", "duplicate name"},
		{"unknown provider type", "providers:\n  - name: a\n    type: cohere\n    base_url: http://x\n", "unknown type"},
		{"model without provider", "models:\n  - id: gpt-5.6\n", "provider is required"},
		{"model unknown provider", "models:\n  - id: gpt-5.6\n    provider: nope\n", "unknown provider"},
		{"duplicate model", "providers:\n  - name: a\nmodels:\n  - id: m\n    provider: a\n  - id: m\n    provider: a\n", "duplicate id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want error containing %q", tc.yaml, tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestProviderKeyPrefersInline(t *testing.T) {
	p := Provider{APIKey: "inline", APIKeyEnv: "CCGW_TEST_UNSET_KEY"}
	if got := p.Key(); got != "inline" {
		t.Errorf("Key() = %q, want inline", got)
	}
}

func TestProviderKeyFromEnv(t *testing.T) {
	t.Setenv("CCGW_TEST_KEY", "from-env")
	p := Provider{APIKeyEnv: "CCGW_TEST_KEY"}
	if got := p.Key(); got != "from-env" {
		t.Errorf("Key() = %q, want from-env", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestProviderTranslationDefaults(t *testing.T) {
	c, err := Parse([]byte("providers:\n  - name: openai\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := c.Providers[0]
	if p.Reasoning != ReasoningAsThinking {
		t.Errorf("Reasoning = %q, want thinking", p.Reasoning)
	}
	if p.MaxTokensField != MaxCompletionTokensField {
		t.Errorf("MaxTokensField = %q, want %q", p.MaxTokensField, MaxCompletionTokensField)
	}
	// Zero value must mean "withhold", so the safe behaviour needs no config.
	if p.KeepPlanTools {
		t.Error("KeepPlanTools should default to false")
	}
}

func TestProviderTranslationOptionsRejectBadValues(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{"reasoning", "providers:\n  - name: a\n    reasoning: loud\n", "unknown reasoning"},
		{"max_tokens_field", "providers:\n  - name: a\n    max_tokens_field: tokens\n", "unknown max_tokens_field"},
		{"negative cap", "providers:\n  - name: a\n    max_tokens_cap: -1\n", "must not be negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !contains(err.Error(), tc.want) {
				t.Errorf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLogBodiesKeyIsGone(t *testing.T) {
	// It was declared but never read. A key that silently does nothing is
	// worse than no key, so it must now be rejected rather than ignored.
	if _, err := Parse([]byte("log_bodies: true\n")); err == nil {
		t.Error("log_bodies should be rejected as an unknown field")
	}
}

func TestCredentialWarningsFlagUnsetEnvVar(t *testing.T) {
	t.Setenv("CCGW_TEST_MISSING_KEY", "")
	c, err := Parse([]byte(`
providers:
  - name: openai
    api_key_env: CCGW_TEST_MISSING_KEY
models:
  - id: gpt-5.6
    provider: openai
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	warns := c.CredentialWarnings()
	if len(warns) != 1 {
		t.Fatalf("CredentialWarnings() = %v, want one", warns)
	}
	if !contains(warns[0], "CCGW_TEST_MISSING_KEY") {
		t.Errorf("warning = %q, want it to name the env var", warns[0])
	}
}

func TestCredentialWarningsSilentWhenKeyIsPresent(t *testing.T) {
	t.Setenv("CCGW_TEST_PRESENT_KEY", "sk-test")
	c, err := Parse([]byte(`
providers:
  - name: openai
    api_key_env: CCGW_TEST_PRESENT_KEY
models:
  - id: gpt-5.6
    provider: openai
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if warns := c.CredentialWarnings(); len(warns) != 0 {
		t.Errorf("CredentialWarnings() = %v, want none", warns)
	}
}

func TestCredentialWarningsIgnoreUnusedProviders(t *testing.T) {
	// A provider with no models routed to it cannot produce a 401, so warning
	// about it would just be noise.
	c, err := Parse([]byte("providers:\n  - name: openai\n    api_key_env: CCGW_TEST_UNUSED\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if warns := c.CredentialWarnings(); len(warns) != 0 {
		t.Errorf("CredentialWarnings() = %v, want none for an unused provider", warns)
	}
}

func TestCredentialWarningsSkipLoopbackBackends(t *testing.T) {
	// A local backend — Ollama, LM Studio, a self-hosted gateway — accepts
	// requests without a credential; warning about it would be noise.
	for _, base := range []string{
		"http://127.0.0.1:8080",
		"http://localhost:11434/v1",
		"http://[::1]:8080",
	} {
		c, err := Parse([]byte(`
providers:
  - name: local
    type: anthropic-compatible
    base_url: ` + base + `
models:
  - id: m
    provider: local
`))
		if err != nil {
			t.Fatalf("Parse(%s): %v", base, err)
		}
		if warns := c.CredentialWarnings(); len(warns) != 0 {
			t.Errorf("base_url %s produced %v, want no warning", base, warns)
		}
	}
}

func TestCredentialWarningsStillFireForRemoteBackends(t *testing.T) {
	c, err := Parse([]byte(`
providers:
  - name: remote
    type: anthropic-compatible
    base_url: https://api.example.com
models:
  - id: m
    provider: remote
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if warns := c.CredentialWarnings(); len(warns) != 1 {
		t.Errorf("CredentialWarnings() = %v, want one for a remote backend", warns)
	}
}

func TestAnthropicCompatibleRejectsOpenAIOnlyOptions(t *testing.T) {
	// Silently ignoring configuration is how the dead log_bodies key happened.
	for _, y := range []string{
		"providers:\n  - name: a\n    type: anthropic-compatible\n    base_url: http://x.test\n    reasoning: drop\n",
		"providers:\n  - name: a\n    type: anthropic-compatible\n    base_url: http://x.test\n    drop_temperature: true\n",
		"providers:\n  - name: a\n    type: anthropic-compatible\n    base_url: http://x.test\n    max_tokens_cap: 100\n",
	} {
		if _, err := Parse([]byte(y)); err == nil || !contains(err.Error(), "does not apply") {
			t.Errorf("Parse(%q) error = %v, want a 'does not apply' rejection", y, err)
		}
	}
}

func TestAnthropicCompatibleRequiresBaseURL(t *testing.T) {
	_, err := Parse([]byte("providers:\n  - name: a\n    type: anthropic-compatible\n"))
	if err == nil || !contains(err.Error(), "base_url is required") {
		t.Errorf("error = %v, want base_url required", err)
	}
}
