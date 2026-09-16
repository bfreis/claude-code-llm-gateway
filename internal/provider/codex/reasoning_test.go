package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

func TestReasoningSignatureRoundTrip(t *testing.T) {
	sig := EncodeReasoning("rs_abc123", "ENCRYPTED==blob:with:colons")
	if !strings.HasPrefix(sig, SignaturePrefix) {
		t.Fatalf("signature = %q, want the gateway prefix", sig)
	}
	id, content, ok := DecodeReasoning(sig)
	if !ok {
		t.Fatal("DecodeReasoning rejected a signature it minted")
	}
	if id != "rs_abc123" {
		t.Errorf("item id = %q", id)
	}
	// The blob contains colons; the separator must stay unambiguous.
	if content != "ENCRYPTED==blob:with:colons" {
		t.Errorf("encrypted content = %q", content)
	}
}

func TestDecodeReasoningRejectsForeignSignatures(t *testing.T) {
	// Anthropic's own signatures must never be mistaken for ours, or we would
	// replay Claude's reasoning to OpenAI as an encrypted blob.
	for _, sig := range []string{
		"",
		"ErUBCkYIBxgCKkDd...", // an Anthropic-shaped signature
		// A foreign signature that *does* contain colons: without the prefix
		// check this would decode into a bogus reasoning item and replay some
		// other tool's state to the backend.
		"othertool:v1:aWQ:their-blob",
		"ccgw:openai:v1:aWQ:not-codex", // another gateway path's prefix
		SignaturePrefix,                // prefix with nothing after it
		SignaturePrefix + "onlyid",
		SignaturePrefix + "!!!notbase64:content",
	} {
		if _, _, ok := DecodeReasoning(sig); ok {
			t.Errorf("DecodeReasoning(%q) accepted a signature it did not mint", sig)
		}
	}
}

func TestEmptyEncryptedContentProducesNoSignature(t *testing.T) {
	if got := EncodeReasoning("rs_1", ""); got != "" {
		t.Errorf("EncodeReasoning with no blob = %q, want empty", got)
	}
}

func TestGatewaySignedThinkingIsStrippedBeforeAnthropic(t *testing.T) {
	// The whole scheme depends on this: our signature is recognisable, so the
	// Anthropic passthrough removes the block rather than letting Anthropic
	// reject it with "Invalid `signature` in `thinking` block".
	sig := EncodeReasoning("rs_1", "blob")
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"x","signature":"` + sig + `"},` +
		`{"type":"text","text":"answer"}]}]}`)

	out, removed := anthropic.StripUnsignedThinking(body)
	if removed != 1 {
		t.Fatalf("removed %d blocks, want 1", removed)
	}
	if strings.Contains(string(out), SignaturePrefix) {
		t.Errorf("a gateway signature survived towards Anthropic:\n%s", out)
	}
}

func TestAnthropicSignedThinkingSurvives(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"x","signature":"ErUBCkYIBxgC"},` +
		`{"type":"text","text":"y"}]}]}`)
	out, removed := anthropic.StripUnsignedThinking(body)
	if removed != 0 || string(out) != string(body) {
		t.Errorf("Anthropic's own signed block must pass through untouched")
	}
}

func TestReasoningReplayedFromTheSignature(t *testing.T) {
	// A previous turn's reasoning comes back through Claude Code as a thinking
	// block; it must become a reasoning item again so the model keeps its chain
	// of thought.
	sig := EncodeReasoning("rs_prev", "ENCRYPTED_BLOB")
	in := &anthropic.MessagesRequest{Messages: []anthropic.Message{
		{Role: anthropic.RoleAssistant, Content: mustJSON(t, []anthropic.ContentBlock{
			{Type: anthropic.BlockThinking, Thinking: "earlier reasoning", Signature: sig},
			{Type: anthropic.BlockText, Text: "earlier answer"},
		})},
	}}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Input) != 2 {
		t.Fatalf("Input = %+v, want a reasoning item then the message", out.Input)
	}
	r := out.Input[0]
	if r.Type != ItemReasoning {
		t.Fatalf("Input[0].Type = %q, want reasoning first", r.Type)
	}
	if r.ID != "rs_prev" || r.EncryptedContent != "ENCRYPTED_BLOB" {
		t.Errorf("reasoning item = %+v", r)
	}
	if out.Input[1].Type != ItemMessage {
		t.Errorf("Input[1].Type = %q", out.Input[1].Type)
	}
}

func TestForeignThinkingIsNotReplayedAsReasoning(t *testing.T) {
	// A Claude-signed thinking block carries nothing this backend can use.
	in := &anthropic.MessagesRequest{Messages: []anthropic.Message{
		{Role: anthropic.RoleAssistant, Content: mustJSON(t, []anthropic.ContentBlock{
			{Type: anthropic.BlockThinking, Thinking: "claude reasoning", Signature: "ErUBCkYIBxgC"},
			{Type: anthropic.BlockText, Text: "answer"},
		})},
	}}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range out.Input {
		if item.Type == ItemReasoning {
			t.Error("a foreign thinking block was replayed as a reasoning item")
		}
	}
}

func TestStreamEmitsEncryptedReasoningAsASignature(t *testing.T) {
	body := sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
		"item": map[string]any{"type": ItemReasoning}}) +
		sse(t, map[string]any{"type": EvReasoningTextDelta, "output_index": 0, "delta": "pondering"}) +
		sse(t, map[string]any{"type": EvOutputItemDone, "output_index": 0,
			"item": map[string]any{"type": ItemReasoning, "id": "rs_9",
				"encrypted_content": "BLOB"}}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"id": "r"}})

	fs := runStream(t, body, StreamOptions{ReasoningMode: ReasoningAsThinking})

	var sig string
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockDelta {
			continue
		}
		if d := f.data["delta"].(map[string]any); d["type"] == anthropic.DeltaSignature {
			sig = d["signature"].(string)
		}
	}
	if sig == "" {
		t.Fatal("no signature_delta emitted; reasoning continuity would be lost")
	}
	id, content, ok := DecodeReasoning(sig)
	if !ok || id != "rs_9" || content != "BLOB" {
		t.Errorf("decoded signature = (%q, %q, %v)", id, content, ok)
	}
}

func TestStreamOmitsSignatureWhenReasoningIsDropped(t *testing.T) {
	body := sse(t, map[string]any{"type": EvOutputItemDone, "output_index": 0,
		"item": map[string]any{"type": ItemReasoning, "id": "rs_9", "encrypted_content": "BLOB"}}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"id": "r"}})

	fs := runStream(t, body, StreamOptions{ReasoningMode: ReasoningDrop})
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockDelta {
			continue
		}
		if d, ok := f.data["delta"].(map[string]any); ok && d["type"] == anthropic.DeltaSignature {
			t.Error("a signature was emitted although reasoning is set to drop")
		}
	}
}

func TestRoundTripSurvivesJSONEncoding(t *testing.T) {
	// The signature travels through Claude Code as JSON; it must not contain
	// anything that mangles on the way.
	sig := EncodeReasoning("rs_1", "a+b/c=d==")
	encoded, err := json.Marshal(map[string]string{"signature": sig})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if _, content, ok := DecodeReasoning(back["signature"]); !ok || content != "a+b/c=d==" {
		t.Errorf("round trip lost the blob: %q", back["signature"])
	}
}
