package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStripUnsignedThinkingLeavesOrdinaryBodiesByteIdentical(t *testing.T) {
	// The common case must not be touched at all: Anthropic's prompt cache
	// keys on the exact bytes.
	raw := []byte(`{"model":"claude-opus-5","z_last":1,"messages":[{"role":"user","content":"hi"}],"a_first":2}`)
	got, n := StripUnsignedThinking(raw)
	if n != 0 {
		t.Fatalf("removed %d blocks, want 0", n)
	}
	if string(got) != string(raw) {
		t.Errorf("body was rewritten:\n got %s\nwant %s", got, raw)
	}
}

func TestStripUnsignedThinkingKeepsSignedBlocks(t *testing.T) {
	// A signed block came from Anthropic and must be replayed untouched.
	raw := []byte(`{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"real","signature":"abc123"},` +
		`{"type":"text","text":"hello"}]}]}`)
	got, n := StripUnsignedThinking(raw)
	if n != 0 {
		t.Fatalf("removed %d blocks, want 0", n)
	}
	if string(got) != string(raw) {
		t.Errorf("signed thinking must survive byte-identical:\n%s", got)
	}
}

func TestStripUnsignedThinkingRemovesUnsignedBlocks(t *testing.T) {
	// This is what the gateway itself synthesises for a non-Anthropic model.
	// Replaying it earns "Invalid `signature` in `thinking` block" (400).
	raw := []byte(`{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"synthesised"},` +
		`{"type":"text","text":"hello"}]}]}`)
	got, n := StripUnsignedThinking(raw)
	if n != 1 {
		t.Fatalf("removed %d blocks, want 1", n)
	}

	var req struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	blocks := req.Messages[0].Content
	if len(blocks) != 1 || blocks[0]["type"] != "text" {
		t.Fatalf("blocks = %v, want just the text block", blocks)
	}
	if strings.Contains(string(got), "synthesised") {
		t.Errorf("thinking text survived:\n%s", got)
	}
}

func TestStripUnsignedThinkingTreatsEmptySignatureAsUnsigned(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"x","signature":""},{"type":"text","text":"y"}]}]}`)
	if _, n := StripUnsignedThinking(raw); n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
}

func TestStripUnsignedThinkingKeepsRedactedThinking(t *testing.T) {
	// redacted_thinking carries `data` rather than a signature and is a
	// different block type; it must not be swept up.
	raw := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"redacted_thinking","data":"enc"},{"type":"text","text":"y"}]}]}`)
	got, n := StripUnsignedThinking(raw)
	if n != 0 {
		t.Fatalf("removed %d, want 0", n)
	}
	if string(got) != string(raw) {
		t.Errorf("redacted_thinking must survive untouched:\n%s", got)
	}
}

func TestStripUnsignedThinkingNeverLeavesAnEmptyMessage(t *testing.T) {
	// A message stripped to zero content blocks is rejected by the API, which
	// would swap one 400 for another.
	raw := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"only"}]}]}`)
	got, n := StripUnsignedThinking(raw)
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	var req struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0]["type"] != "text" {
		t.Errorf("content = %v, want a placeholder text block", req.Messages[0].Content)
	}
}

func TestStripUnsignedThinkingFailsOpen(t *testing.T) {
	// Anything unparseable or unexpected is forwarded as-is, so the worst case
	// is the behaviour we had before: a 400 and Claude Code's own recovery.
	for _, raw := range []string{
		`not json at all "thinking"`,
		`{"messages":"thinking"}`,
		`{"messages":[{"role":"user","content":"a string mentioning thinking"}]}`,
		`{"no_messages":1,"x":"thinking"}`,
	} {
		got, n := StripUnsignedThinking([]byte(raw))
		if n != 0 || string(got) != raw {
			t.Errorf("StripUnsignedThinking(%q) = (%q, %d), want it untouched", raw, got, n)
		}
	}
}

func TestStripUnsignedThinkingHandlesSeveralMessages(t *testing.T) {
	raw := []byte(`{"messages":[` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"a"},{"type":"text","text":"1"}]},` +
		`{"role":"user","content":"q"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"b","signature":"sig"},{"type":"text","text":"2"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"c"},{"type":"text","text":"3"}]}]}`)
	got, n := StripUnsignedThinking(raw)
	if n != 2 {
		t.Fatalf("removed %d, want 2 (the signed one stays)", n)
	}
	if !strings.Contains(string(got), `"sig"`) {
		t.Errorf("the signed block was dropped:\n%s", got)
	}
}
