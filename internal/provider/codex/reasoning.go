package codex

import (
	"encoding/base64"
	"strings"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

// SignaturePrefix marks a thinking-block signature this gateway minted.
//
// Anthropic signs its own thinking blocks and rejects a replayed one whose
// signature it did not issue, so this prefix has to be recognisable on the way
// back out: a block carrying it must never be forwarded to Anthropic.
const SignaturePrefix = anthropic.GatewaySignaturePrefix + "codex:v1:"

// EncodeReasoning packs a Codex reasoning item into an Anthropic thinking
// block's signature field.
//
// The Responses API returns reasoning as an opaque encrypted blob that should
// be replayed on the following turn to preserve the model's chain of thought.
// This gateway is stateless — it rebuilds the request from the conversation
// Claude Code replays — so there is nowhere to keep that blob between turns.
// Smuggling it through `signature` solves that without a session store:
// measured against Claude Code 2.1.273, the field survives replay verbatim.
//
// The item id travels alongside because the API identifies the reasoning item
// by it, and base64 keeps the separator unambiguous.
func EncodeReasoning(itemID, encryptedContent string) string {
	if encryptedContent == "" {
		return ""
	}
	return SignaturePrefix +
		base64.RawURLEncoding.EncodeToString([]byte(itemID)) + ":" +
		encryptedContent
}

// DecodeReasoning unpacks a signature written by EncodeReasoning. ok is false
// for any signature this gateway did not mint, including Anthropic's own.
func DecodeReasoning(signature string) (itemID, encryptedContent string, ok bool) {
	rest, found := strings.CutPrefix(signature, SignaturePrefix)
	if !found {
		return "", "", false
	}
	encodedID, content, found := strings.Cut(rest, ":")
	if !found || content == "" {
		return "", "", false
	}
	id, err := base64.RawURLEncoding.DecodeString(encodedID)
	if err != nil {
		return "", "", false
	}
	return string(id), content, true
}

// IsGatewaySignature reports whether a signature was minted here.
//
// The Anthropic passthrough uses this to drop these blocks before they reach
// Anthropic, which would reject them.
func IsGatewaySignature(signature string) bool {
	return strings.HasPrefix(signature, SignaturePrefix)
}
