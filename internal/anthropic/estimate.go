package anthropic

import "encoding/json"

// TextBytes sums the natural-language content of a Messages request: system
// prompt, message content, and tool schemas. Structural JSON overhead and
// escaping are deliberately excluded to keep the estimate close to what a
// tokenizer would count.
func TextBytes(req *MessagesRequest) int {
	total := blockBytes(req.System)
	for _, m := range req.Messages {
		total += blockBytes(m.Content)
	}
	for _, t := range req.Tools {
		total += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return total
}

// blockBytes counts the text in a string-or-blocks content field.
func blockBytes(raw json.RawMessage) int {
	blocks, err := DecodeContent(raw)
	if err != nil {
		return len(raw)
	}
	var total int
	for _, b := range blocks {
		switch b.Type {
		case BlockText:
			total += len(b.Text)
		case BlockThinking:
			total += len(b.Thinking)
		case BlockToolUse:
			total += len(b.Name) + len(b.Input)
		case BlockToolResult:
			total += blockBytes(b.Content)
		case BlockImage, BlockDocument:
			// Base64 payloads are not text and would swamp the estimate; they
			// are billed by dimensions, which the gateway cannot see.
			if b.Source != nil {
				total += len(b.Source.URL)
			}
		}
	}
	return total
}

// EstimateInputTokens approximates the prompt size of a parsed Messages
// request, roughly four characters per token for English prose.
//
// A translated backend (OpenAI, Codex) only learns real input-token usage
// once its stream completes, so message_start would otherwise report zero —
// which Claude Code renders as context usage briefly dropping to zero every
// turn before jumping back up at message_delta. Seeding message_start with
// this estimate keeps the number in the right neighborhood the whole time;
// the final message_delta still corrects it to the exact reported count.
func EstimateInputTokens(req *MessagesRequest) int {
	if n := TextBytes(req) / 4; n > 0 {
		return n
	}
	return 1
}
