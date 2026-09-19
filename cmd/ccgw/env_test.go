package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
	"github.com/bfreis/claude-code-llm-gateway/internal/picker"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = saved
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	if runErr != nil {
		t.Fatalf("command failed: %v", runErr)
	}
	return string(out)
}

func envFor(t *testing.T, models string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "listen: 127.0.0.1:8787\nproviders:\n  - name: codex\n    type: codex\nmodels:\n" + models
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return captureStdout(t, func() error {
		return cmdEnv(append([]string{"-config", path}, args...))
	})
}

func TestEnvExportsTheMeasuredWindow(t *testing.T) {
	// Writing context_window into the config is pointless unless the variable
	// Claude Code reads actually reaches the shell.
	out := envFor(t, "  - id: a\n    provider: codex\n    context_window: 920000\n")
	if !strings.Contains(out, "export CLAUDE_CODE_MAX_CONTEXT_TOKENS=920000") {
		t.Errorf("no window export in:\n%s", out)
	}
}

func TestEnvExportsNothingWithoutAMeasuredWindow(t *testing.T) {
	out := envFor(t, "  - id: a\n    provider: codex\n")
	if strings.Contains(out, "MAX_CONTEXT_TOKENS") || strings.Contains(out, "AUTO_COMPACT_WINDOW") {
		t.Errorf("exported a window nobody measured:\n%s", out)
	}
}

func TestEnvUsesAutoCompactWindowForLongContext(t *testing.T) {
	out := envFor(t, "  - id: a\n    provider: codex\n    long_context: true\n    context_window: 920000\n")
	if !strings.Contains(out, "export CLAUDE_CODE_AUTO_COMPACT_WINDOW=920000") {
		t.Errorf("wrong variable for a [1m] model:\n%s", out)
	}
	if strings.Contains(out, "export CLAUDE_CODE_MAX_CONTEXT_TOKENS") {
		t.Errorf("exported a variable the [1m] suffix makes inert:\n%s", out)
	}
}

func TestEnvWarnsWhenModelsDisagree(t *testing.T) {
	// The lever is process-global, so the user needs to know the larger model
	// is being held to the smaller one's window.
	out := envFor(t, "  - id: a\n    provider: codex\n    context_window: 920000\n"+
		"  - id: b\n    provider: codex\n    context_window: 400000\n")
	if !strings.Contains(out, "export CLAUDE_CODE_MAX_CONTEXT_TOKENS=400000") {
		t.Errorf("did not take the smallest window:\n%s", out)
	}
	if !strings.Contains(out, "920000") {
		t.Errorf("did not say which window is being given up:\n%s", out)
	}
}

func TestEnvExportsTheWindowInEveryMode(t *testing.T) {
	// The window is a property of the models, not of how Claude Code discovers
	// them, so it must not be lost by choosing a different mode.
	for _, mode := range []string{"fidelity", "discovery", "gateway"} {
		out := envFor(t, "  - id: a\n    provider: codex\n    context_window: 920000\n", "-mode", mode)
		if !strings.Contains(out, "export CLAUDE_CODE_MAX_CONTEXT_TOKENS=920000") {
			t.Errorf("mode %s lost the window export:\n%s", mode, out)
		}
	}
}

func TestEnvExplainsTheClaudeModelWindowFallback(t *testing.T) {
	// Losing 1M on Claude models is caused by ANTHROPIC_BASE_URL alone, so it
	// happens in every mode and with no provider models configured at all.
	// It looks exactly like the gateway breaking Claude, so it has to be said.
	for _, mode := range []string{"fidelity", "discovery", "gateway"} {
		out := envFor(t, "  - id: a\n    provider: codex\n", "-mode", mode)
		if !strings.Contains(out, "sonnet[1m]") {
			t.Errorf("mode %s does not say how to restore the 1M window:\n%s", mode, out)
		}
		if !strings.Contains(out, "200000") {
			t.Errorf("mode %s does not say what the window falls back to:\n%s", mode, out)
		}
	}
}

func TestTheClaudeNoteIsShellSafe(t *testing.T) {
	// The whole output is eval'd, so every explanatory line must be a comment.
	out := envFor(t, "  - id: a\n    provider: codex\n    context_window: 920000\n")
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(trimmed, "export ") && !strings.HasPrefix(trimmed, "unset ") &&
			!strings.HasPrefix(trimmed, "alias ") {
			t.Errorf("line is neither comment nor assignment, so eval would run it: %q", line)
		}
	}
}

func TestEnvEnablesToolSearch(t *testing.T) {
	// Behind a non-first-party base URL Claude Code disables tool search and
	// inlines every MCP schema instead - 191k of context in a real session.
	out := envFor(t, "  - id: a\n    provider: codex\n")
	if !strings.Contains(out, "export ENABLE_TOOL_SEARCH=true") {
		t.Errorf("tool search not enabled:\n%s", out)
	}
}

func TestEnvUnsetsToolSearchWhenDisabled(t *testing.T) {
	// Leaving a stale value from the shell would defeat the config.
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "listen: 127.0.0.1:8787\nenable_tool_search: false\nproviders:\n  - name: codex\n" +
		"    type: codex\nmodels:\n  - id: a\n    provider: codex\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error { return cmdEnv([]string{"-config", path}) })
	if strings.Contains(out, "export ENABLE_TOOL_SEARCH") {
		t.Errorf("exported tool search although it is disabled:\n%s", out)
	}
	if !strings.Contains(out, "unset ENABLE_TOOL_SEARCH") {
		t.Errorf("did not clear a value inherited from the shell:\n%s", out)
	}
}

// firstPartyEnv renders 'ccgw env' for a config with assume_first_party on.
func firstPartyEnv(t *testing.T, models string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "listen: 127.0.0.1:8787\nassume_first_party: true\nproviders:\n  - name: codex\n" +
		"    type: codex\nmodels:\n" + models
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return captureStdout(t, func() error {
		return cmdEnv(append([]string{"-config", path}, args...))
	})
}

func TestEnvSetsTheFirstPartyFlag(t *testing.T) {
	// The whole point: without this variable Claude Code declines to fetch
	// remote managed settings behind a custom base URL at all.
	out := firstPartyEnv(t, "  - id: a\n    provider: codex\n")
	if !strings.Contains(out, "export _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1") {
		t.Errorf("flag not exported:\n%s", out)
	}
	if !strings.Contains(out, "export ANTHROPIC_BASE_URL=http://127.0.0.1:8787") {
		t.Errorf("lost the base URL, which the flag only reclassifies:\n%s", out)
	}
}

func TestEnvClearsGatewayDiscoveryUnderTheFlag(t *testing.T) {
	// Claude Code requires the base URL *not* to be first-party for discovery,
	// so leaving the variable exported would advertise a picker that is gone.
	out := firstPartyEnv(t, "  - id: a\n    provider: codex\n")
	if strings.Contains(out, "export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY") {
		t.Errorf("exported a discovery flag the first-party flag makes inert:\n%s", out)
	}
	if !strings.Contains(out, "unset CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY") {
		t.Errorf("did not clear a value inherited from the shell:\n%s", out)
	}
}

func TestEnvKeepsOneModelSelectableUnderTheFlag(t *testing.T) {
	// The discovered rows are gone, so without this there is no way to reach a
	// provider model from /model at all.
	out := firstPartyEnv(t, "  - id: a\n    provider: codex\n    display_name: Model A\n")
	if !strings.Contains(out, "export ANTHROPIC_CUSTOM_MODEL_OPTION='anthropic/a'") {
		t.Errorf("no surviving model row:\n%s", out)
	}
	if !strings.Contains(out, "export ANTHROPIC_CUSTOM_MODEL_OPTION_NAME='Model A'") {
		t.Errorf("row has no label:\n%s", out)
	}
}

func TestEnvRestoresEveryModelUnderTheFlag(t *testing.T) {
	// Losing gateway discovery must not mean losing the catalogue: the curated
	// settings file carries all of it, and the alias is the only thing that
	// makes Claude Code read the file.
	out := firstPartyEnv(t, "  - id: a\n    provider: codex\n  - id: b\n    provider: codex\n")
	if !strings.Contains(out, "alias claude=") {
		t.Errorf("no alias, so the curated rows never reach Claude Code:\n%s", out)
	}
	if !strings.Contains(out, "--settings") {
		t.Errorf("alias does not pass the settings file:\n%s", out)
	}
	if !strings.Contains(out, "model-picker.settings.json") {
		t.Errorf("alias does not name the file ccgw writes:\n%s", out)
	}
	if !strings.Contains(out, "2 provider model(s)") {
		t.Errorf("does not say how many models the file carries:\n%s", out)
	}
}

func TestTheAliasSurvivesAShell(t *testing.T) {
	// An alias whose quoting is wrong fails at eval time, or silently drops
	// the flag - either way the rows are gone and the cause is invisible.
	dir := t.TempDir()
	path := filepath.Join(dir, "sub dir", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "listen: 127.0.0.1:8787\nassume_first_party: true\nproviders:\n  - name: codex\n" +
		"    type: codex\nmodels:\n  - id: a\n    provider: codex\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error { return cmdEnv([]string{"-config", path}) })

	// A path with a space in it is the case naive quoting gets wrong, so the
	// check is what the shell resolves the alias to, not what was printed.
	got, err := exec.Command("/bin/sh", "-c", "set -eu\n"+out+"\ncommand -v alias >/dev/null; alias claude").Output()
	if err != nil {
		t.Fatalf("eval of 'ccgw env' failed: %v\n%s", err, out)
	}
	want := filepath.Join(dir, "sub dir", "model-picker.settings.json")
	if !strings.Contains(string(got), want) {
		t.Errorf("alias resolves to %q, which does not carry %q", got, want)
	}
}

func TestEnvClearsTheCustomOptionWithNoProviderModels(t *testing.T) {
	// A stale value from the shell would pin a model the config no longer has.
	out := firstPartyEnv(t, "  []\n")
	if strings.Contains(out, "export ANTHROPIC_CUSTOM_MODEL_OPTION") {
		t.Errorf("exported a row for a model that does not exist:\n%s", out)
	}
	if !strings.Contains(out, "unset ANTHROPIC_CUSTOM_MODEL_OPTION") {
		t.Errorf("did not clear the inherited value:\n%s", out)
	}
}

func TestEnvDropsTheOneMillionWorkaroundUnderTheFlag(t *testing.T) {
	// The [1m] advice exists only because a foreign base URL costs Claude
	// models their native window. The flag removes the cause, so repeating the
	// workaround would send people to a suffix they no longer need.
	out := firstPartyEnv(t, "  - id: a\n    provider: codex\n")
	if strings.Contains(out, "/model sonnet[1m]") {
		t.Errorf("still prescribes the [1m] workaround:\n%s", out)
	}
	if !strings.Contains(out, "native 1M window") {
		t.Errorf("does not say the window is intact:\n%s", out)
	}
}

func TestEnvRefusesTheFlagInModesThatNeedDiscovery(t *testing.T) {
	// Both modes are built on the discovery the flag switches off, so the
	// combination cannot work and must not look like it did.
	for _, mode := range []string{"discovery", "gateway"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		body := "listen: 127.0.0.1:8787\nassume_first_party: true\nproviders:\n  - name: codex\n" +
			"    type: codex\nmodels:\n  - id: a\n    provider: codex\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		err := cmdEnv([]string{"-config", path, "-mode", mode})
		if err == nil {
			t.Errorf("mode %s accepted assume_first_party", mode)
			continue
		}
		if !strings.Contains(err.Error(), "assume_first_party") {
			t.Errorf("mode %s: error does not name the cause: %v", mode, err)
		}
	}
}

func TestTheFirstPartyEnvIsShellSafe(t *testing.T) {
	// The output is eval'd and display names come from the config, so a quote
	// in one must not end the assignment. Run it through a real shell rather
	// than asserting on the rendering: what matters is what eval does with it.
	out := firstPartyEnv(t, "  - id: a\n    provider: codex\n"+
		"    display_name: \"OpenAI's $HOME; echo pwned\"\n")
	script := "set -euo pipefail\n" + out + "\nprintf '%s' \"$ANTHROPIC_CUSTOM_MODEL_OPTION_NAME\"\n"
	got, err := exec.Command("/bin/sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("eval of 'ccgw env' failed: %v\n%s", err, out)
	}
	if want := "OpenAI's $HOME; echo pwned"; string(got) != want {
		t.Errorf("shell saw %q, want %q", got, want)
	}
}

// configAt writes a config and returns its path.
func configAt(t *testing.T, firstParty bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "listen: 127.0.0.1:8787\n"
	if firstParty {
		body += "assume_first_party: true\n"
	}
	body += "providers:\n  - name: codex\n    type: codex\nmodels:\n" +
		"  - id: a\n    provider: codex\n    display_name: A\n  - id: b\n    provider: codex\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyncPickerWritesCuratedSettingsUnderTheFlag(t *testing.T) {
	// Writing the discovery cache here would report success for a file Claude
	// Code does not read, which is the failure this whole path exists to avoid.
	// $HOME is redirected so a stray cache write would land in the temp dir
	// rather than the real ~/.claude, and can be asserted absent.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	cfgPath := configAt(t, true)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := syncPicker(cfg, cfgPath)
	if err != nil {
		t.Fatalf("syncPicker: %v", err)
	}
	if want := picker.SettingsPath(cfgPath); res.Path != want {
		t.Errorf("wrote %q, want the curated settings at %q", res.Path, want)
	}
	if res.Count != 2 {
		t.Errorf("Count = %d, want both models", res.Count)
	}
	got, err := picker.ReadSettings(res.Path)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if len(got.ModelPicker.Options) != 2 || got.ModelPicker.Options[0].Model != "anthropic/a" {
		t.Errorf("curated options are wrong: %+v", got.ModelPicker.Options)
	}
	cachePath, err := picker.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("also wrote the discovery cache at %s, which nothing reads under the flag", cachePath)
	}
}

func TestSyncPickerWritesTheCacheByDefault(t *testing.T) {
	// The default path must be untouched by any of this.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	cfgPath := configAt(t, false)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := syncPicker(cfg, cfgPath)
	if err != nil {
		t.Fatalf("syncPicker: %v", err)
	}
	if want, _ := picker.Path(); res.Path != want {
		t.Errorf("wrote %q, want the discovery cache at %q", res.Path, want)
	}
	if _, err := os.Stat(picker.SettingsPath(cfgPath)); !os.IsNotExist(err) {
		t.Errorf("wrote curated settings without the flag, which would need --settings to matter")
	}
}
