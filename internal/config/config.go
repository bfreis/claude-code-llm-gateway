// Package config loads and validates the gateway's YAML configuration.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AuthMode describes how the gateway authenticates to an upstream Anthropic API.
type AuthMode string

const (
	// AuthPassthrough forwards whatever credential Claude Code sent. This is
	// what keeps a Claude subscription in play: the OAuth token Claude Code
	// would have sent to api.anthropic.com is relayed untouched.
	AuthPassthrough AuthMode = "passthrough"
	// AuthBearer sends a gateway-held bearer token (e.g. from `claude setup-token`).
	AuthBearer AuthMode = "bearer"
	// AuthAPIKey sends a gateway-held x-api-key.
	AuthAPIKey AuthMode = "api_key"
)

// DefaultAliasPrefix is prepended to non-Anthropic model IDs.
//
// Claude Code's gateway model discovery drops any ID that does not match
// /(claude|anthropic)/i, so a bare "gpt-5.6" never reaches the /model picker.
// Prefixing makes it "anthropic/gpt-5.6", which survives. The prefix is
// stripped again before the request leaves for the real provider.
const DefaultAliasPrefix = "anthropic/"

// Config is the top-level gateway configuration.
type Config struct {
	// Listen is the host:port the gateway binds. Loopback only by default.
	Listen string `yaml:"listen"`
	// AliasPrefix is prepended to non-Anthropic model IDs so that Claude
	// Code's discovery filter accepts them. Set to "" to disable.
	AliasPrefix *string `yaml:"alias_prefix"`
	// Anthropic configures the default passthrough route.
	Anthropic AnthropicConfig `yaml:"anthropic"`
	// Providers are the non-Anthropic backends.
	Providers []Provider `yaml:"providers"`
	// Models is the catalogue advertised on GET /v1/models.
	Models []Model `yaml:"models"`
}

// AnthropicConfig describes the first-party Anthropic backend, which every
// model ID not claimed by a configured provider falls through to.
type AnthropicConfig struct {
	BaseURL string   `yaml:"base_url"`
	Auth    AuthMode `yaml:"auth"`
	// TokenEnv names an env var holding a bearer token (auth: bearer).
	TokenEnv string `yaml:"token_env"`
	// APIKeyEnv names an env var holding an API key (auth: api_key).
	APIKeyEnv string `yaml:"api_key_env"`
	// AddBetas are anthropic-beta values merged into every proxied request.
	//
	// Gateway mode sends a reduced beta set: measured against Claude Code
	// 2.1.273, it drops oauth-2025-04-20, context-management-2025-06-27,
	// prompt-caching-scope-2026-01-05, extended-cache-ttl-2025-04-11 and
	// others that base-URL mode sends. Listing them here puts them back.
	AddBetas []string `yaml:"add_betas"`
}

// OAuthBeta is required by the Anthropic API when the credential is a
// subscription OAuth token rather than an API key. Gateway mode does not send
// it, so the gateway adds it whenever it forwards such a token.
const OAuthBeta = "oauth-2025-04-20"

// Provider types.
const (
	// TypeOpenAI translates to the OpenAI Chat Completions API.
	TypeOpenAI = "openai"
	// TypeCodex talks to a ChatGPT subscription directly, the way the official
	// Codex CLI does: OAuth credentials, the ChatGPT Responses endpoint, and
	// translation performed here. No API key and no second process.
	TypeCodex = "codex"
	// TypeAnthropicCompatible forwards the Anthropic Messages request as-is to
	// a backend that already speaks it: another gateway, a self-hosted
	// endpoint, anything exposing the Messages API. No translation happens
	// here; the backend does it, which is what makes tool use work against
	// endpoints whose OpenAI-compatible route does not support function calls.
	TypeAnthropicCompatible = "anthropic-compatible"
)

// Provider is a non-Anthropic backend.
type Provider struct {
	Name string `yaml:"name"`
	// Type selects how requests reach this backend: "openai" (default) or
	// "anthropic-compatible".
	Type string `yaml:"type"`
	// BaseURL is the API root, e.g. https://api.openai.com/v1
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names the env var holding the provider's API key.
	APIKeyEnv string `yaml:"api_key_env"`
	// APIKey is an inline key. Prefer APIKeyEnv.
	APIKey string `yaml:"api_key"`
	// Headers are extra headers sent upstream.
	Headers map[string]string `yaml:"headers"`

	// Reasoning decides what becomes of reasoning text the backend emits:
	// "thinking" (default) surfaces it as Anthropic thinking blocks, "drop"
	// discards it.
	Reasoning ReasoningMode `yaml:"reasoning"`
	// MaxTokensField selects which output-cap field to send.
	// "max_completion_tokens" (default) is required by OpenAI reasoning
	// models; "max_tokens" suits older or third-party OpenAI-compatible APIs.
	MaxTokensField string `yaml:"max_tokens_field"`
	// DropTemperature omits temperature and top_p. OpenAI reasoning models
	// reject any temperature other than their default, so set this when
	// pointing at one.
	DropTemperature bool `yaml:"drop_temperature"`
	// MaxTokensCap clamps the output cap for every model on this provider.
	// 0 means no clamp.
	MaxTokensCap int `yaml:"max_tokens_cap"`
	// AuthPath overrides where the ChatGPT credential is read and written
	// (type: codex). Defaults to $CODEX_HOME/auth.json, else ~/.codex/auth.json.
	AuthPath string `yaml:"auth_path"`
	// ServiceTier is passed through on codex requests; "priority" is the
	// faster tier the CLI's -fast model forms select.
	ServiceTier string `yaml:"service_tier"`
	// ClientVersion is the Codex CLI version reported to the backend
	// (type: codex). The backend gates model availability on it, refusing a
	// newer model with "requires a newer version of Codex". Set this to what
	// `codex --version` prints if a model you expect is refused.
	ClientVersion string `yaml:"client_version"`
	// KeepPlanTools forwards Claude Code's plan-mode tools.
	//
	// By default they are withheld: EnterPlanMode and ExitPlanMode are part of
	// Claude Code's own workflow, and non-Claude models call them
	// unprompted, which derails a turn.
	KeepPlanTools bool `yaml:"keep_plan_tools"`
}

// ReasoningMode selects the handling of upstream reasoning text.
type ReasoningMode string

const (
	// ReasoningAsThinking surfaces reasoning as Anthropic thinking blocks.
	ReasoningAsThinking ReasoningMode = "thinking"
	// ReasoningDrop discards reasoning text.
	ReasoningDrop ReasoningMode = "drop"
)

// Output-cap field names.
const (
	MaxCompletionTokensField = "max_completion_tokens"
	MaxTokensField           = "max_tokens"
)

// Model is one entry in the advertised catalogue.
type Model struct {
	// ID is the provider's own model ID, e.g. "gpt-5.6".
	ID string `yaml:"id"`
	// Provider names a Provider by its Name.
	Provider string `yaml:"provider"`
	// DisplayName is what Claude Code's picker shows. Defaults to ID.
	DisplayName string `yaml:"display_name"`
	// Description is the picker's secondary line.
	Description string `yaml:"description"`
	// MaxTokens caps max_tokens for this model when Claude Code asks for more.
	MaxTokens int `yaml:"max_tokens"`
	// LongContext advertises the model with a [1m] suffix.
	//
	// Claude Code assumes a 200k window for a model it does not recognise; the
	// suffix raises its client-side window to 1M. It is a claim about the
	// backend, not a request to it: the suffix is stripped before the model ID
	// is forwarded. Only set it if the model really accepts that much input.
	LongContext bool `yaml:"long_context"`
}

// Defaults applied when the YAML omits them.
const (
	DefaultListen           = "127.0.0.1:8787"
	DefaultAnthropicBaseURL = "https://api.anthropic.com"
	DefaultOpenAIBaseURL    = "https://api.openai.com/v1"
)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse validates YAML config bytes.
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.AliasPrefix == nil {
		p := DefaultAliasPrefix
		c.AliasPrefix = &p
	}
	if c.Anthropic.BaseURL == "" {
		c.Anthropic.BaseURL = DefaultAnthropicBaseURL
	}
	if c.Anthropic.Auth == "" {
		c.Anthropic.Auth = AuthPassthrough
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Type == "" {
			p.Type = TypeOpenAI
		}
		if p.Type == TypeCodex && p.Reasoning == "" {
			p.Reasoning = ReasoningAsThinking
		}
		if p.Type == TypeOpenAI {
			if p.Reasoning == "" {
				p.Reasoning = ReasoningAsThinking
			}
			if p.MaxTokensField == "" {
				p.MaxTokensField = MaxCompletionTokensField
			}
			if p.BaseURL == "" {
				p.BaseURL = DefaultOpenAIBaseURL
			}
		}
	}
}

func (c *Config) validate() error {
	switch c.Anthropic.Auth {
	case AuthPassthrough:
	case AuthBearer:
		if c.Anthropic.TokenEnv == "" {
			return fmt.Errorf("anthropic.auth=bearer requires anthropic.token_env")
		}
	case AuthAPIKey:
		if c.Anthropic.APIKeyEnv == "" {
			return fmt.Errorf("anthropic.auth=api_key requires anthropic.api_key_env")
		}
	default:
		return fmt.Errorf("anthropic.auth: unknown mode %q (want passthrough, bearer or api_key)", c.Anthropic.Auth)
	}

	seenProvider := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider: name is required")
		}
		if seenProvider[p.Name] {
			return fmt.Errorf("provider %q: duplicate name", p.Name)
		}
		seenProvider[p.Name] = true
		switch p.Type {
		case TypeOpenAI, TypeAnthropicCompatible, TypeCodex:
		default:
			return fmt.Errorf("provider %q: unknown type %q (want %s, %s or %s)",
				p.Name, p.Type, TypeOpenAI, TypeAnthropicCompatible, TypeCodex)
		}
		if p.Type == TypeCodex {
			// The endpoint is fixed by the service, and the credential is an
			// OAuth token rather than a key, so neither is required here.
			switch p.Reasoning {
			case ReasoningAsThinking, ReasoningDrop:
			default:
				return fmt.Errorf("provider %q: unknown reasoning %q (want thinking or drop)", p.Name, p.Reasoning)
			}
			if p.APIKey != "" || p.APIKeyEnv != "" {
				return fmt.Errorf("provider %q: a codex provider signs in with OAuth, so api_key does not apply - run 'ccgw codex login'", p.Name)
			}
			continue
		}
		if p.BaseURL == "" {
			return fmt.Errorf("provider %q: base_url is required", p.Name)
		}
		if p.Type == TypeAnthropicCompatible {
			// These describe the OpenAI translation, which this type does not
			// perform. Rejecting them beats silently ignoring configuration.
			for _, set := range []struct {
				name string
				on   bool
			}{
				{"reasoning", p.Reasoning != ""},
				{"max_tokens_field", p.MaxTokensField != ""},
				{"drop_temperature", p.DropTemperature},
				{"max_tokens_cap", p.MaxTokensCap != 0},
			} {
				if set.on {
					return fmt.Errorf("provider %q: %s does not apply to an %s provider, which does no translation",
						p.Name, set.name, TypeAnthropicCompatible)
				}
			}
			continue
		}
		switch p.Reasoning {
		case ReasoningAsThinking, ReasoningDrop:
		default:
			return fmt.Errorf("provider %q: unknown reasoning %q (want thinking or drop)", p.Name, p.Reasoning)
		}
		switch p.MaxTokensField {
		case MaxCompletionTokensField, MaxTokensField:
		default:
			return fmt.Errorf("provider %q: unknown max_tokens_field %q (want %s or %s)",
				p.Name, p.MaxTokensField, MaxCompletionTokensField, MaxTokensField)
		}
		if p.MaxTokensCap < 0 {
			return fmt.Errorf("provider %q: max_tokens_cap must not be negative", p.Name)
		}
	}

	seenModel := map[string]bool{}
	for _, m := range c.Models {
		if m.ID == "" {
			return fmt.Errorf("model: id is required")
		}
		if seenModel[m.ID] {
			return fmt.Errorf("model %q: duplicate id", m.ID)
		}
		seenModel[m.ID] = true
		if m.Provider == "" {
			return fmt.Errorf("model %q: provider is required", m.ID)
		}
		if !seenProvider[m.Provider] {
			return fmt.Errorf("model %q: references unknown provider %q", m.ID, m.Provider)
		}
	}
	return nil
}

// Prefix returns the configured alias prefix.
func (c *Config) Prefix() string {
	if c.AliasPrefix == nil {
		return DefaultAliasPrefix
	}
	return *c.AliasPrefix
}

// ProviderByName looks up a configured provider.
func (c *Config) ProviderByName(name string) (*Provider, bool) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// Key resolves a provider's API key from its inline value or env var.
func (p *Provider) Key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return ""
}

// CredentialWarnings names providers that will send no Authorization header.
//
// This is a warning rather than an error because a local backend — Ollama, LM
// Studio, vLLM — legitimately needs no key. It exists so that a provider
// configured to read a key from an env var that turns out to be unset is
// reported at startup, instead of surfacing much later as an opaque 401 the
// first time someone picks that model.
func (c *Config) CredentialWarnings() []string {
	used := map[string]bool{}
	for _, m := range c.Models {
		used[m.Provider] = true
	}

	var out []string
	for i := range c.Providers {
		p := &c.Providers[i]
		if !used[p.Name] || p.Key() != "" || p.Type == TypeCodex {
			continue
		}
		// A backend on loopback — Ollama, LM Studio, a self-hosted endpoint —
		// is expected to need no credential, so saying so every time would be
		// noise in the most common local setup.
		if isLoopback(p.BaseURL) {
			continue
		}
		switch {
		case p.APIKeyEnv != "":
			out = append(out, fmt.Sprintf(
				"provider %q: %s is unset, so requests go out unauthenticated. "+
					"Hosted APIs will reject them; a local backend may be fine.",
				p.Name, p.APIKeyEnv))
		default:
			out = append(out, fmt.Sprintf(
				"provider %q: no api_key or api_key_env configured, so requests go out "+
					"unauthenticated. Hosted APIs will reject them; a local backend may be fine.",
				p.Name))
		}
	}
	return out
}

// isLoopback reports whether a base URL points at this machine.
func isLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
