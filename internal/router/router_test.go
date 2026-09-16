package router

import (
	"strings"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
)

func testRouter(t *testing.T, yaml string) *Router {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return New(cfg)
}

const sampleConfig = `
providers:
  - name: openai
    base_url: https://api.openai.com/v1
models:
  - id: gpt-5.6
    provider: openai
    display_name: GPT-5.6
  - id: claude-ish-model
    provider: openai
`

func TestExposedIDPrefixesOnlyWhenNeeded(t *testing.T) {
	r := testRouter(t, sampleConfig)
	if got := r.ExposedID("gpt-5.6"); got != "anthropic/gpt-5.6" {
		t.Errorf("ExposedID(gpt-5.6) = %q, want anthropic/gpt-5.6", got)
	}
	// Already satisfies Claude Code's /(claude|anthropic)/i filter.
	if got := r.ExposedID("claude-ish-model"); got != "claude-ish-model" {
		t.Errorf("ExposedID(claude-ish-model) = %q, want it unprefixed", got)
	}
}

func TestResolveProviderByPrefixedAndBareID(t *testing.T) {
	r := testRouter(t, sampleConfig)
	for _, id := range []string{"anthropic/gpt-5.6", "gpt-5.6"} {
		got := r.Resolve(id)
		if got.Kind != KindProvider {
			t.Fatalf("Resolve(%q).Kind = %v, want KindProvider", id, got.Kind)
		}
		if got.UpstreamModel != "gpt-5.6" {
			t.Errorf("Resolve(%q).UpstreamModel = %q, want gpt-5.6", id, got.UpstreamModel)
		}
		if got.Provider == nil || got.Provider.Name != "openai" {
			t.Errorf("Resolve(%q).Provider = %+v, want openai", id, got.Provider)
		}
	}
}

func TestResolveUnknownFallsThroughToAnthropic(t *testing.T) {
	r := testRouter(t, sampleConfig)
	got := r.Resolve("claude-opus-5")
	if got.Kind != KindAnthropic {
		t.Fatalf("Kind = %v, want KindAnthropic", got.Kind)
	}
	if got.UpstreamModel != "claude-opus-5" {
		t.Errorf("UpstreamModel = %q, want claude-opus-5", got.UpstreamModel)
	}
}

func TestResolveKeepsVariantSuffixForAnthropic(t *testing.T) {
	r := testRouter(t, sampleConfig)
	got := r.Resolve("claude-sonnet-5[1m]")
	if got.Kind != KindAnthropic {
		t.Fatalf("Kind = %v, want KindAnthropic", got.Kind)
	}
	// Anthropic parses the suffix itself, so it must survive untouched.
	if got.UpstreamModel != "claude-sonnet-5[1m]" {
		t.Errorf("UpstreamModel = %q, want the suffix preserved", got.UpstreamModel)
	}
}

func TestResolveStripsVariantSuffixForProviderModels(t *testing.T) {
	r := testRouter(t, sampleConfig)
	got := r.Resolve("anthropic/gpt-5.6[1m]")
	if got.Kind != KindProvider {
		t.Fatalf("Kind = %v, want KindProvider", got.Kind)
	}
	if got.UpstreamModel != "gpt-5.6" {
		t.Errorf("UpstreamModel = %q, want gpt-5.6 without the variant marker", got.UpstreamModel)
	}
}

func TestCatalogueUsesExposedIDsAndDefaultsDisplayName(t *testing.T) {
	r := testRouter(t, sampleConfig)
	cat := r.Catalogue()
	if len(cat) != 2 {
		t.Fatalf("len(Catalogue()) = %d, want 2", len(cat))
	}
	if cat[0].ID != "anthropic/gpt-5.6" || cat[0].DisplayName != "GPT-5.6" {
		t.Errorf("cat[0] = %+v", cat[0])
	}
	if cat[1].DisplayName != "claude-ish-model" {
		t.Errorf("cat[1].DisplayName = %q, want it to default to the ID", cat[1].DisplayName)
	}
}

func TestSplitVariant(t *testing.T) {
	tests := []struct {
		in         string
		base, var_ string
		ok         bool
	}{
		{"claude-sonnet-5[1m]", "claude-sonnet-5", "1m", true},
		{"gpt-5.6", "gpt-5.6", "", false},
		{"[1m]", "[1m]", "", false},
	}
	for _, tc := range tests {
		base, v, ok := splitVariant(tc.in)
		if base != tc.base || v != tc.var_ || ok != tc.ok {
			t.Errorf("splitVariant(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.in, base, v, ok, tc.base, tc.var_, tc.ok)
		}
	}
}

func TestDiscoveryWarningsFlagUnprefixableIDs(t *testing.T) {
	// With prefixing off, a bare provider ID is silently dropped by Claude
	// Code's discovery filter. The user should hear about it, not wonder why
	// the picker is empty.
	r := testRouter(t, `
alias_prefix: ""
providers:
  - name: openai
models:
  - id: gpt-5.6
    provider: openai
  - id: claude-ish
    provider: openai
`)
	warns := r.DiscoveryWarnings()
	if len(warns) != 1 {
		t.Fatalf("DiscoveryWarnings() = %v, want exactly one", warns)
	}
	if !strings.Contains(warns[0], "gpt-5.6") {
		t.Errorf("warning = %q, want it to name gpt-5.6", warns[0])
	}
}

func TestDiscoveryWarningsSilentWithDefaultPrefix(t *testing.T) {
	r := testRouter(t, sampleConfig)
	if warns := r.DiscoveryWarnings(); len(warns) != 0 {
		t.Errorf("DiscoveryWarnings() = %v, want none: the default prefix makes every ID acceptable", warns)
	}
}

func TestCatalogueAppendsLongContextSuffix(t *testing.T) {
	r := testRouter(t, `
providers:
  - name: openai
models:
  - id: gpt-5.6
    provider: openai
    long_context: true
  - id: gpt-5.6-mini
    provider: openai
`)
	cat := r.Catalogue()
	if cat[0].ID != "anthropic/gpt-5.6[1m]" {
		t.Errorf("cat[0].ID = %q, want the [1m] suffix", cat[0].ID)
	}
	if cat[1].ID != "anthropic/gpt-5.6-mini" {
		t.Errorf("cat[1].ID = %q, want no suffix", cat[1].ID)
	}
}

func TestResolveStripsLongContextSuffixBeforeTheProvider(t *testing.T) {
	// Whatever the picker advertises must route, and the provider must never
	// see Claude Code's client-side window hint.
	r := testRouter(t, `
providers:
  - name: openai
models:
  - id: gpt-5.6
    provider: openai
    long_context: true
`)
	got := r.Resolve(r.Catalogue()[0].ID)
	if got.Kind != KindProvider {
		t.Fatalf("Kind = %v, want KindProvider", got.Kind)
	}
	if got.UpstreamModel != "gpt-5.6" {
		t.Errorf("UpstreamModel = %q, want gpt-5.6", got.UpstreamModel)
	}
}
