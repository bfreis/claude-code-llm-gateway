package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		if !strings.HasPrefix(trimmed, "export ") && !strings.HasPrefix(trimmed, "unset ") {
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
