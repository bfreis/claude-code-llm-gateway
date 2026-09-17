package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
	"github.com/bfreis/claude-code-llm-gateway/internal/picker"
	"github.com/bfreis/claude-code-llm-gateway/internal/provider/codex"
)

func TestDisplayNameIsReadable(t *testing.T) {
	tests := map[string]string{
		"gpt-5.6-terra": "GPT 5.6 Terra (Codex)",
		"gpt-5.6":       "GPT 5.6 (Codex)",
		"gpt-5.6-sol":   "GPT 5.6 Sol (Codex)",
		"o3-mini":       "o3 Mini (Codex)",
		"custom_model":  "Custom Model (Codex)",
	}
	for id, want := range tests {
		if got := displayName(id, "Codex"); got != want {
			t.Errorf("displayName(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestCodexConfiguredModelIsRead(t *testing.T) {
	// The Codex CLI's own config names a model the account is known to serve,
	// which is what lets setup avoid asking.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	body := "# a comment\napproval_policy = \"on-request\"\nmodel = \"gpt-5.6-terra\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexConfiguredModel(); got != "gpt-5.6-terra" {
		t.Errorf("codexConfiguredModel() = %q", got)
	}
}

func TestCodexConfiguredModelAbsent(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	if got := codexConfiguredModel(); got != "" {
		t.Errorf("codexConfiguredModel() = %q, want empty when there is no config", got)
	}
}

func TestVersionPatternHandlesRealCodexOutput(t *testing.T) {
	tests := map[string]string{
		"codex-cli 0.155.0-alpha.11\n": "0.155.0-alpha.11",
		"codex 0.154.0\n":              "0.154.0",
		"nonsense":                     "",
	}
	for in, want := range tests {
		if got := versionPattern.FindString(in); got != want {
			t.Errorf("version of %q = %q, want %q", in, got, want)
		}
	}
}

// renderedConfigLoads is the property that matters most: whatever setup writes
// must be a config this same binary accepts, or the wizard has handed the user
// a broken file.
func renderedConfigLoads(t *testing.T, a answers) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(renderConfig(a)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("generated config does not load: %v\n---\n%s", err, renderConfig(a))
	}
	return cfg
}

func TestGeneratedConfigLoadsForEveryCombination(t *testing.T) {
	cases := map[string]answers{
		"nothing but anthropic": {listen: "127.0.0.1:8787"},
		"codex only": {
			listen: "127.0.0.1:8787", useCodex: true,
			codexVersion: "0.155.0-alpha.11", codexModels: []string{"gpt-5.6-terra"},
		},
		"openai only": {
			listen: "127.0.0.1:8787", useOpenAI: true,
			openAIKeyEnv: "OPENAI_API_KEY", openAIModels: []string{"gpt-5.6"},
		},
		"both": {
			listen:   "127.0.0.1:8787",
			useCodex: true, codexVersion: "0.154.0", codexModels: []string{"gpt-5.6-sol", "gpt-5.6-terra"},
			useOpenAI: true, openAIKeyEnv: "OPENAI_API_KEY", openAIModels: []string{"gpt-5.6"},
		},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := renderedConfigLoads(t, a)
			if cfg.Listen != a.listen {
				t.Errorf("Listen = %q", cfg.Listen)
			}
			want := len(a.codexModels) + len(a.openAIModels)
			if len(cfg.Models) != want {
				t.Errorf("models = %d, want %d", len(cfg.Models), want)
			}
		})
	}
}

func TestGeneratedCodexConfigCarriesTheDetectedVersion(t *testing.T) {
	// The version is the difference between a model working and being refused,
	// so it must reach the file rather than fall back to the built-in default.
	cfg := renderedConfigLoads(t, answers{
		listen: "127.0.0.1:8787", useCodex: true,
		codexVersion: "0.155.0-alpha.11", codexModels: []string{"gpt-5.6-terra"},
	})
	p, ok := cfg.ProviderByName("codex")
	if !ok {
		t.Fatal("no codex provider in the generated config")
	}
	if p.ClientVersion != "0.155.0-alpha.11" {
		t.Errorf("client_version = %q, want the detected one", p.ClientVersion)
	}
}

func TestGeneratedConfigQuotesAwkwardModelIDs(t *testing.T) {
	// A bare 5.6 would parse as a float and an ID with a colon would break the
	// mapping, so IDs are quoted.
	a := answers{listen: "127.0.0.1:8787", useCodex: true,
		codexVersion: "0.154.0", codexModels: []string{"gpt-5.6", "vendor:model-1"}}
	cfg := renderedConfigLoads(t, a)
	ids := make([]string, len(cfg.Models))
	for i, m := range cfg.Models {
		ids[i] = m.ID
	}
	if strings.Join(ids, ",") != "gpt-5.6,vendor:model-1" {
		t.Errorf("model ids = %v", ids)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a , b ,, c ")
	if strings.Join(got, "|") != "a|b|c" {
		t.Errorf("splitList = %v", got)
	}
	if splitList("  ") != nil {
		t.Error("an empty list should produce no entries")
	}
}

func TestSetupConfiguresTheWholeCodexCatalogue(t *testing.T) {
	// Astra, Sol, Terra and Luna should all be offered without being asked for.
	ids := codex.DefaultModelIDs()
	for _, want := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if !contains(ids, want) {
			t.Errorf("default catalogue is missing %q: %v", want, ids)
		}
	}
	cfg := renderedConfigLoads(t, answers{
		listen: "127.0.0.1:8787", useCodex: true,
		codexVersion: "0.154.0", codexModels: ids,
	})
	if len(cfg.Models) != len(ids) {
		t.Fatalf("configured %d models, want %d", len(cfg.Models), len(ids))
	}
	// Curated labels beat ones derived from the ID.
	for _, m := range cfg.Models {
		if m.DisplayName != codex.LabelFor(m.ID) {
			t.Errorf("%s label = %q, want the curated %q", m.ID, m.DisplayName, codex.LabelFor(m.ID))
		}
		if m.Description == "" {
			t.Errorf("%s has no description", m.ID)
		}
	}
}

func TestCodexCLIsOwnModelIsAddedWhenUnknown(t *testing.T) {
	// If someone's Codex CLI is set to a model outside the shipped list, they
	// clearly use it, so it belongs in the picker too.
	ids := append(codex.DefaultModelIDs(), "gpt-7")
	cfg := renderedConfigLoads(t, answers{
		listen: "127.0.0.1:8787", useCodex: true, codexVersion: "0.154.0", codexModels: ids,
	})
	var found bool
	for _, m := range cfg.Models {
		if m.ID == "gpt-7" {
			found = true
			// No curated label exists, so one is derived.
			if m.DisplayName != "GPT 7 (Codex)" {
				t.Errorf("derived label = %q", m.DisplayName)
			}
		}
	}
	if !found {
		t.Error("the CLI's own model was not configured")
	}
}

func TestSetupSyncsThePickerCache(t *testing.T) {
	// Without this, a gateway already running with the previous config keeps
	// serving the old picker list: the models just configured never appear,
	// which looks exactly like setup having done nothing.
	claudeHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	// A signed-in Codex home, so setup actually configures models. Without one
	// the catalogue is empty and the assertion below would hold trivially.
	t.Setenv("CODEX_HOME", signedInCodexHome(t))

	// Drive the real command rather than calling syncPicker directly: the
	// property under test is that *setup* refreshes the cache, not that the
	// sync helper works.
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := cmdSetup([]string{"-y", "-config", cfgPath, "-listen", "127.0.0.1:8787"}); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("setup wrote a config that does not load: %v", err)
	}

	path, err := picker.Path()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := picker.Read(path)
	if err != nil {
		t.Fatalf("no picker cache written: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) == 0 {
		t.Fatal("setup configured no models, so this test would prove nothing")
	}
	if len(cache.Models) != len(cfg.Models) {
		t.Errorf("cache lists %d models but the config setup wrote has %d",
			len(cache.Models), len(cfg.Models))
	}
	// The reader compares baseUrl by string equality and ignores a mismatch.
	if cache.BaseURL != "http://127.0.0.1:8787" {
		t.Errorf("cache baseUrl = %q", cache.BaseURL)
	}
	for _, m := range cache.Models {
		if !strings.HasPrefix(m.ID, "anthropic/") {
			t.Errorf("cached id %q lacks the prefix Claude Code's filter requires", m.ID)
		}
	}
}

func TestSameModelIDsDetectsDrift(t *testing.T) {
	a := []picker.Model{{ID: "anthropic/one"}, {ID: "anthropic/two"}}
	if !sameModelIDs(a, []picker.Model{{ID: "anthropic/one"}, {ID: "anthropic/two"}}) {
		t.Error("identical lists reported as drifted")
	}
	for _, other := range [][]picker.Model{
		{{ID: "anthropic/one"}},
		{{ID: "anthropic/one"}, {ID: "anthropic/three"}},
		{{ID: "anthropic/two"}, {ID: "anthropic/one"}},
	} {
		if sameModelIDs(a, other) {
			t.Errorf("drift not detected against %v", other)
		}
	}
}

// signedInCodexHome builds a CODEX_HOME whose credential the store accepts, so
// setup takes its "already signed in" path without any network or browser.
func signedInCodexHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	jwt := func(payload map[string]any) string {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return enc([]byte(`{"alg":"none"}`)) + "." + enc(body) + ".sig"
	}
	auth := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": jwt(map[string]any{"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "acct_test", "chatgpt_plan_type": "pro"}}),
			"access_token":  jwt(map[string]any{"exp": float64(time.Now().Add(24 * time.Hour).Unix())}),
			"refresh_token": "rt",
			"account_id":    "acct_test",
		},
	}
	body, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestCodexModelsCarryTheMeasuredWindow(t *testing.T) {
	// Without it every Codex row sits at the 200k Claude Code assumes for an ID
	// it does not recognise, so auto-compact fires at a fraction of the window
	// the account actually serves.
	cfg := renderedConfigLoads(t, answers{
		listen: "127.0.0.1:8787", useCodex: true, codexWindow: codex.DefaultContextWindow,
		codexVersion: "0.154.0", codexModels: codex.DefaultModelIDs(),
	})
	for _, m := range cfg.Models {
		if m.ContextWindow != codex.DefaultContextWindow {
			t.Errorf("%s context_window = %d, want %d", m.ID, m.ContextWindow, codex.DefaultContextWindow)
		}
		// The [1m] suffix would pin Claude Code at 1M and make the variable
		// that carries the real number inert.
		if m.LongContext {
			t.Errorf("%s was advertised long_context, which overstates the window", m.ID)
		}
	}
}

func TestGeneratedConfigYieldsTheRightDirective(t *testing.T) {
	// The config is only useful if it produces the export Claude Code reads.
	cfg := renderedConfigLoads(t, answers{
		listen: "127.0.0.1:8787", useCodex: true, codexWindow: codex.DefaultContextWindow,
		codexVersion: "0.154.0", codexModels: codex.DefaultModelIDs(),
	})
	d, ok := cfg.WindowDirective()
	if !ok {
		t.Fatal("generated config produces no window directive")
	}
	if d.Name != config.EnvMaxContextTokens || d.Value != codex.DefaultContextWindow {
		t.Errorf("directive = %s=%d", d.Name, d.Value)
	}
	if d.Mixed {
		t.Error("a single-window catalogue was reported as mixed")
	}
}

func TestWindowCanBeLeftUnstated(t *testing.T) {
	// It is a claim about the backend, so it has to be possible to not make it.
	cfg := renderedConfigLoads(t, answers{
		listen: "127.0.0.1:8787", useCodex: true, codexWindow: 0,
		codexVersion: "0.154.0", codexModels: codex.DefaultModelIDs(),
	})
	for _, m := range cfg.Models {
		if m.ContextWindow != 0 {
			t.Errorf("%s declares a window although none was asked for", m.ID)
		}
	}
	if _, ok := cfg.WindowDirective(); ok {
		t.Error("a directive was produced with no measured window")
	}
}

func TestSetupStatesTheWindowByDefault(t *testing.T) {
	// The default matters more than the field: a user who runs `ccgw setup` and
	// never opens the file should get the window their subscription serves.
	cfg := setupThenLoad(t, "-y", "-listen", "127.0.0.1:8787")
	for _, m := range cfg.Models {
		if m.ContextWindow != codex.DefaultContextWindow {
			t.Errorf("%s context_window = %d, want %d", m.ID, m.ContextWindow, codex.DefaultContextWindow)
		}
	}
}

func TestSetupHonoursAnExplicitWindow(t *testing.T) {
	cfg := setupThenLoad(t, "-y", "-codex-window", "400000", "-listen", "127.0.0.1:8787")
	for _, m := range cfg.Models {
		if m.ContextWindow != 400000 {
			t.Errorf("%s context_window = %d, want the one given on the command line", m.ID, m.ContextWindow)
		}
	}
}

// setupThenLoad runs the real command against a signed-in fixture and returns
// the config it wrote, failing if it configured nothing to assert about.
func setupThenLoad(t *testing.T, args ...string) *config.Config {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", signedInCodexHome(t))
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := cmdSetup(append([]string{"-config", cfgPath}, args...)); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("setup wrote a config that does not load: %v", err)
	}
	if len(cfg.Models) == 0 {
		t.Fatal("setup configured no models, so this test would prove nothing")
	}
	return cfg
}

func TestSetupRefusesToClobberAndSaysHow(t *testing.T) {
	// -y means "take the detected defaults", and the default answer to
	// "Replace it?" is no - so -y alone can never replace a config. Failing is
	// right; failing without naming the flag that works is not.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", signedInCodexHome(t))
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	existing := "listen: 127.0.0.1:9999\n"
	if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	err := cmdSetup([]string{"-y", "-config", cfgPath, "-listen", "127.0.0.1:8787"})
	if err == nil {
		t.Fatal("setup replaced an existing config without being asked to")
	}
	if !strings.Contains(err.Error(), "-force") {
		t.Errorf("error does not name the flag that works: %v", err)
	}
	raw, readErr := os.ReadFile(cfgPath)
	if readErr != nil || string(raw) != existing {
		t.Errorf("the existing config was not left alone: %q", string(raw))
	}
}

func TestSetupForceKeepsABackup(t *testing.T) {
	// setup regenerates from detection, so replacing a hand-tuned config is a
	// real way to lose work.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", signedInCodexHome(t))
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	existing := "listen: 127.0.0.1:9999  # hand tuned\n"
	if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cmdSetup([]string{"-y", "-force", "-config", cfgPath, "-listen", "127.0.0.1:8787"}); err != nil {
		t.Fatalf("cmdSetup -force: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("setup wrote a config that does not load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8787" {
		t.Errorf("Listen = %q, want the new config", cfg.Listen)
	}
	backup, err := os.ReadFile(cfgPath + ".bak")
	if err != nil {
		t.Fatalf("no backup of the replaced config: %v", err)
	}
	if string(backup) != existing {
		t.Errorf("backup = %q, want the file that was replaced", string(backup))
	}
}

func TestSetupTurnsToolSearchOn(t *testing.T) {
	// Inlined MCP schemas cost more context than everything else the gateway
	// touches, so a generated config should not leave the saving behind.
	cfg := setupThenLoad(t, "-y", "-listen", "127.0.0.1:8787")
	if !cfg.ToolSearchEnabled() {
		t.Error("the generated config does not enable tool search")
	}
	if cfg.EnableToolSearch == nil {
		t.Error("tool search is left to the default rather than written out where it can be seen")
	}
}
