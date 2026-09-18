package openai

import (
	"encoding/json"
	"fmt"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

// TranslateResponse converts a non-streaming OpenAI completion into an
// Anthropic Messages response. modelID is echoed back as Claude Code sent it,
// so the UI keeps showing the name the user picked.
func TranslateResponse(in *ChatResponse, modelID string, opt Options) (*anthropic.MessagesResponse, error) {
	out := &anthropic.MessagesResponse{
		ID:    messageID(in.ID),
		Type:  "message",
		Role:  anthropic.RoleAssistant,
		Model: modelID,
	}
	if in.Usage != nil {
		out.Usage = translateUsage(in.Usage)
	}
	if len(in.Choices) == 0 {
		out.StopReason = anthropic.StopEndTurn
		out.Content = []anthropic.ContentBlock{}
		return out, nil
	}

	ch := in.Choices[0]
	if r := ch.Message.ReasoningText(); r != "" && opt.ReasoningMode == ReasoningAsThinking {
		out.Content = append(out.Content, anthropic.ContentBlock{
			Type:     anthropic.BlockThinking,
			Thinking: r,
		})
	}
	if text, err := messageText(ch.Message.Content); err != nil {
		return nil, err
	} else if text != "" {
		out.Content = append(out.Content, anthropic.ContentBlock{Type: anthropic.BlockText, Text: text})
	}
	for _, tc := range ch.Message.ToolCalls {
		out.Content = append(out.Content, anthropic.ContentBlock{
			Type:  anthropic.BlockToolUse,
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: argumentsJSON(tc.Function.Arguments),
		})
	}
	if out.Content == nil {
		out.Content = []anthropic.ContentBlock{}
	}
	out.StopReason = TranslateStopReason(ch.FinishReason, len(ch.Message.ToolCalls) > 0)
	return out, nil
}

// messageText extracts the text of an OpenAI message, which is either a plain
// string or an array of content parts.
func messageText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("decode message content: %w", err)
	}
	var out string
	for _, p := range parts {
		if p.Type == "text" {
			out += p.Text
		}
	}
	return out, nil
}

// argumentsJSON normalises a tool call's argument string into valid JSON.
//
// A model that is cut off mid-call leaves a truncated fragment here; an empty
// object is a better answer than invalid JSON in the response body.
func argumentsJSON(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage(`{}`)
	}
	if !json.Valid([]byte(args)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(args)
}

// TranslateStopReason maps an OpenAI finish_reason onto Anthropic's vocabulary.
func TranslateStopReason(finish string, hasToolCalls bool) string {
	switch finish {
	case "stop":
		return anthropic.StopEndTurn
	case "length":
		return anthropic.StopMaxTokens
	case "tool_calls", "function_call":
		return anthropic.StopToolUse
	case "content_filter":
		return anthropic.StopEndTurn
	case "":
		if hasToolCalls {
			return anthropic.StopToolUse
		}
		return anthropic.StopEndTurn
	default:
		if hasToolCalls {
			return anthropic.StopToolUse
		}
		return anthropic.StopEndTurn
	}
}

func translateUsage(u *ChatUsage) anthropic.Usage {
	out := anthropic.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		out.CacheReadInputTokens = u.PromptTokensDetails.CachedTokens
		// Anthropic reports uncached input separately from cache reads.
		out.InputTokens = u.PromptTokens - u.PromptTokensDetails.CachedTokens
		if out.InputTokens < 0 {
			out.InputTokens = 0
		}
	}
	return out
}

// messageID gives the response an Anthropic-shaped id. It must differ between
// responses even when the backend sends none, so the empty case is random
// rather than constant — see anthropic.NewMessageID.
func messageID(openaiID string) string {
	return anthropic.NewMessageID(openaiID)
}
