package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
)

// GatewaySignaturePrefix marks a thinking-block signature this gateway minted
// rather than received from Anthropic.
//
// A provider backend's reasoning rides out to Claude Code in the signature
// field so it can be replayed on the next turn without server-side session
// state. Anthropic did not issue those signatures and rejects them, so they
// must be removed before a request reaches it.
const GatewaySignaturePrefix = "ccgw:"

// StripUnsignedThinking removes assistant thinking blocks that Anthropic will
// not accept, returning the cleaned body and how many blocks it dropped.
//
// Anthropic signs every thinking block it produces and rejects a replayed one
// whose signature is missing or wrong:
//
//	400 … Invalid `signature` in `thinking` block
//
// Claude Code recovers by stripping *all* thinking blocks and retrying
// ("[thinking] server rejected a thinking block; stripping all thinking blocks
// and retrying"), so the turn still completes — but it costs a wasted round
// trip and throws away Claude's own signed reasoning along with the offending
// block.
//
// Two kinds qualify, and both arise the same way — a non-Anthropic model routed
// through this gateway produced them and Claude Code replayed them when the user
// switched back to a Claude model:
//
//   - blocks with no signature at all;
//   - blocks whose signature this gateway minted, carrying a backend's own
//     reasoning state (see GatewaySignaturePrefix).
//
// Dropping exactly those, and only when they are present, avoids the 400
// without touching anything else.
//
// The body is returned unchanged when there is nothing to remove, when it does
// not parse, or when it is not shaped as expected — failing open leaves the
// previous behaviour (a 400 and Claude Code's own recovery) rather than risking
// a corrupted request.
func StripUnsignedThinking(raw []byte) ([]byte, int) {
	// Cheap gate: the overwhelming majority of requests carry no thinking
	// block at all, and those must reach Anthropic byte-for-byte because the
	// prompt cache keys on the exact bytes.
	if !bytes.Contains(raw, []byte(`"thinking"`)) {
		return raw, 0
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal(raw, &req); err != nil {
		return raw, 0
	}
	rawMessages, ok := req["messages"]
	if !ok {
		return raw, 0
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return raw, 0
	}

	removed := 0
	for i := range messages {
		content, ok := messages[i]["content"]
		if !ok {
			continue
		}
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(content, &blocks); err != nil {
			continue // a plain string content holds no thinking blocks
		}

		kept := blocks[:0]
		for _, b := range blocks {
			if isUnsignedThinking(b) {
				removed++
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) == len(blocks) {
			continue
		}
		// An assistant turn stripped down to nothing would be an invalid
		// message, so leave a placeholder rather than an empty block list.
		if len(kept) == 0 {
			kept = append(kept, map[string]json.RawMessage{
				"type": json.RawMessage(`"text"`),
				"text": json.RawMessage(`"(thinking omitted)"`),
			})
		}
		encoded, err := json.Marshal(kept)
		if err != nil {
			return raw, 0
		}
		messages[i]["content"] = encoded
	}

	if removed == 0 {
		return raw, 0
	}

	encodedMessages, err := json.Marshal(messages)
	if err != nil {
		return raw, 0
	}
	req["messages"] = encodedMessages

	// Re-marshalling reorders the top-level keys, which would normally be a
	// prompt-cache concern. It is not one here: a request carrying an unsigned
	// thinking block was going to be rejected and retried anyway, so it had no
	// cache value to lose.
	out, err := json.Marshal(req)
	if err != nil {
		return raw, 0
	}
	return out, removed
}

// isUnsignedThinking reports whether a content block is a thinking block
// Anthropic would reject: no signature, or one this gateway minted.
// redacted_thinking carries `data` instead and is left alone.
func isUnsignedThinking(block map[string]json.RawMessage) bool {
	rawType, ok := block["type"]
	if !ok {
		return false
	}
	var blockType string
	if err := json.Unmarshal(rawType, &blockType); err != nil {
		return false
	}
	if blockType != BlockThinking {
		return false
	}
	sig, ok := block["signature"]
	if !ok {
		return true
	}
	var signature string
	if err := json.Unmarshal(sig, &signature); err != nil {
		return true
	}
	return signature == "" || strings.HasPrefix(signature, GatewaySignaturePrefix)
}
