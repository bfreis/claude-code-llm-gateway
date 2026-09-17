package config_test

import (
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
)

func windowConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	return cfg
}

const windowPreamble = `listen: 127.0.0.1:8787
providers:
  - name: codex
    type: codex
models:
`

func TestNoWindowDirectiveWithoutAMeasuredWindow(t *testing.T) {
	cfg := windowConfig(t, windowPreamble+`  - id: a
    provider: codex
`)
	if d, ok := cfg.WindowDirective(); ok {
		t.Errorf("got %+v, want no directive when no model declares a window", d)
	}
}

func TestWindowDirectiveUsesMaxContextTokens(t *testing.T) {
	// Without a [1m] suffix in play, this is the variable Claude Code reads,
	// and it leaves proxied Claude models alone.
	cfg := windowConfig(t, windowPreamble+`  - id: a
    provider: codex
    context_window: 920000
`)
	d, ok := cfg.WindowDirective()
	if !ok {
		t.Fatal("no directive")
	}
	if d.Name != config.EnvMaxContextTokens || d.Value != 920000 {
		t.Errorf("got %s=%d", d.Name, d.Value)
	}
	if d.Mixed || d.Clamped {
		t.Errorf("unexpected flags: %+v", d)
	}
}

func TestLongContextSwitchesToAutoCompactWindow(t *testing.T) {
	// A [1m] suffix pins Claude Code's cap at 1M before it ever consults
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS, so that variable would be inert here.
	cfg := windowConfig(t, windowPreamble+`  - id: a
    provider: codex
    context_window: 920000
    long_context: true
`)
	d, ok := cfg.WindowDirective()
	if !ok {
		t.Fatal("no directive")
	}
	if d.Name != config.EnvAutoCompactWindow || d.Value != 920000 {
		t.Errorf("got %s=%d", d.Name, d.Value)
	}
}

func TestSmallestWindowWinsAndIsReported(t *testing.T) {
	// The lever is process-global, so a mixed catalogue has to take the
	// smallest window or the largest model overflows.
	cfg := windowConfig(t, windowPreamble+`  - id: a
    provider: codex
    context_window: 920000
  - id: b
    provider: codex
    context_window: 400000
`)
	d, _ := cfg.WindowDirective()
	if d.Value != 400000 {
		t.Errorf("Value = %d, want the smallest", d.Value)
	}
	if !d.Mixed {
		t.Error("a mixed catalogue was not reported as mixed")
	}
	if d.Largest != 920000 {
		t.Errorf("Largest = %d, want the biggest window so it can be named", d.Largest)
	}
}

func TestAutoCompactWindowIsClampedToWhatItCanExpress(t *testing.T) {
	// Claude Code raises this variable to 100k and caps it at 1M, so a value
	// outside that range is not the window the user would actually get.
	for _, tc := range []struct{ give, want int }{{50000, 100000}, {2000000, 1000000}} {
		cfg := windowConfig(t, windowPreamble+`  - id: a
    provider: codex
    long_context: true
    context_window: `+itoa(tc.give)+"\n")
		d, _ := cfg.WindowDirective()
		if d.Value != tc.want {
			t.Errorf("context_window %d -> %d, want %d", tc.give, d.Value, tc.want)
		}
		if !d.Clamped {
			t.Errorf("context_window %d was clamped without saying so", tc.give)
		}
	}
}

func TestMaxContextTokensIsNotClamped(t *testing.T) {
	// This variable has no such range, so a small window is expressible.
	cfg := windowConfig(t, windowPreamble+`  - id: a
    provider: codex
    context_window: 50000
`)
	d, _ := cfg.WindowDirective()
	if d.Value != 50000 || d.Clamped {
		t.Errorf("got %d clamped=%v, want 50000 unclamped", d.Value, d.Clamped)
	}
}

func TestNegativeContextWindowIsRejected(t *testing.T) {
	if _, err := config.Parse([]byte(windowPreamble + `  - id: a
    provider: codex
    context_window: -1
`)); err == nil {
		t.Error("a negative context_window was accepted")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func TestToolSearchDefaultsOn(t *testing.T) {
	// Inlining every MCP schema costs far more context than anything else the
	// gateway does, so the saving is the default rather than an opt-in.
	cfg := windowConfig(t, windowPreamble+"  - id: a\n    provider: codex\n")
	if !cfg.ToolSearchEnabled() {
		t.Error("tool search is off by default")
	}
}

func TestToolSearchCanBeTurnedOff(t *testing.T) {
	// A backend that rejects the request shape fails every turn with a 400, so
	// switching it off must be possible without editing anything else.
	cfg := windowConfig(t, "listen: 127.0.0.1:8787\nenable_tool_search: false\n"+
		"providers:\n  - name: codex\n    type: codex\nmodels:\n  - id: a\n    provider: codex\n")
	if cfg.ToolSearchEnabled() {
		t.Error("enable_tool_search: false was ignored")
	}
}

func TestToolSearchCanBeSetExplicitlyOn(t *testing.T) {
	cfg := windowConfig(t, "listen: 127.0.0.1:8787\nenable_tool_search: true\n"+
		"providers:\n  - name: codex\n    type: codex\nmodels:\n  - id: a\n    provider: codex\n")
	if !cfg.ToolSearchEnabled() {
		t.Error("enable_tool_search: true was ignored")
	}
}
