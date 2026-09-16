package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

// Options tune the translation for a particular OpenAI-compatible backend.
type Options struct {
	// UseMaxCompletionTokens sends max_completion_tokens instead of the
	// legacy max_tokens. Required by OpenAI reasoning models.
	UseMaxCompletionTokens bool
	// DropTemperature omits temperature and top_p. Reasoning models reject
	// any temperature other than the default.
	DropTemperature bool
	// MaxTokensCap clamps the requested output cap. 0 means no clamp.
	MaxTokensCap int
	// ReasoningMode decides what happens to reasoning text the backend emits.
	ReasoningMode ReasoningMode
	// DropPlanTools withholds Claude Code's plan-mode tools from the backend.
	DropPlanTools bool
}

// PlanTools are Claude Code's plan-mode tools.
//
// They drive Claude Code's own plan/accept workflow rather than doing any work,
// and a non-Claude model handed them will call them unprompted and stall the
// turn. Withholding them is the default.
var PlanTools = map[string]bool{
	"EnterPlanMode": true,
	"ExitPlanMode":  true,
}

// ReasoningMode selects the handling of upstream reasoning text.
type ReasoningMode string

const (
	// ReasoningAsThinking surfaces reasoning as Anthropic thinking blocks.
	ReasoningAsThinking ReasoningMode = "thinking"
	// ReasoningDrop discards reasoning text.
	ReasoningDrop ReasoningMode = "drop"
)

// TranslateRequest converts an Anthropic Messages request into an OpenAI Chat
// Completions request. model is the upstream model ID, already stripped of the
// gateway's alias prefix.
func TranslateRequest(in *anthropic.MessagesRequest, model string, opt Options) (*ChatRequest, error) {
	out := &ChatRequest{
		Model:  model,
		Stream: in.Stream,
		Stop:   in.StopSequences,
	}
	if in.Stream {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	maxTokens := in.MaxTokens
	if opt.MaxTokensCap > 0 && (maxTokens == 0 || maxTokens > opt.MaxTokensCap) {
		maxTokens = opt.MaxTokensCap
	}
	if maxTokens > 0 {
		if opt.UseMaxCompletionTokens {
			out.MaxCompletionTokens = maxTokens
		} else {
			out.MaxTokens = maxTokens
		}
	}
	if !opt.DropTemperature {
		out.Temperature = in.Temperature
		out.TopP = in.TopP
	}
	if in.Thinking.Enabled() {
		out.ReasoningEffort = effortFor(in.Thinking.BudgetTokens)
	}

	if sys, err := systemMessage(in.System); err != nil {
		return nil, err
	} else if sys != nil {
		out.Messages = append(out.Messages, *sys)
	}

	for i, m := range in.Messages {
		msgs, err := translateMessage(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}

	for _, t := range in.Tools {
		// Server-side tool types (web_search and friends) have no OpenAI
		// equivalent and carry no schema; skipping them is better than
		// sending a malformed function definition.
		if t.Type != "" && t.InputSchema == nil {
			continue
		}
		if opt.DropPlanTools && PlanTools[t.Name] {
			continue
		}
		out.Tools = append(out.Tools, ChatTool{
			Type: "function",
			Function: ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	tc, err := translateToolChoice(in.ToolChoice)
	if err != nil {
		return nil, err
	}
	out.ToolChoice = tc

	return out, nil
}

// effortFor maps an Anthropic thinking budget onto OpenAI's coarse knob.
func effortFor(budget int) string {
	switch {
	case budget <= 0:
		return ""
	case budget <= 4096:
		return "low"
	case budget <= 16384:
		return "medium"
	default:
		return "high"
	}
}

func systemMessage(raw json.RawMessage) (*ChatMsg, error) {
	blocks, err := anthropic.DecodeContent(raw)
	if err != nil {
		return nil, fmt.Errorf("system: %w", err)
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == anthropic.BlockText && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(b.Text)
		}
	}
	if sb.Len() == 0 {
		return nil, nil
	}
	content, err := json.Marshal(sb.String())
	if err != nil {
		return nil, err
	}
	return &ChatMsg{Role: RoleSystem, Content: content}, nil
}

// translateMessage converts one Anthropic message. It can produce several
// OpenAI messages: Anthropic packs tool results into a user message, while
// OpenAI needs a separate tool-role message per result.
func translateMessage(m anthropic.Message) ([]ChatMsg, error) {
	blocks, err := m.Blocks()
	if err != nil {
		return nil, err
	}

	// Tool results come first so they directly follow the assistant message
	// whose tool_calls they answer.
	var out []ChatMsg
	for _, b := range blocks {
		if b.Type != anthropic.BlockToolResult {
			continue
		}
		text, err := toolResultText(b)
		if err != nil {
			return nil, err
		}
		content, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		out = append(out, ChatMsg{Role: RoleTool, ToolCallID: b.ToolUseID, Content: content})
	}

	if m.Role == anthropic.RoleAssistant {
		msg, ok, err := assistantMessage(blocks)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, msg)
		}
		return out, nil
	}

	msg, ok, err := userMessage(blocks)
	if err != nil {
		return nil, err
	}
	if ok {
		out = append(out, msg)
	}
	return out, nil
}

func assistantMessage(blocks []anthropic.ContentBlock) (ChatMsg, bool, error) {
	msg := ChatMsg{Role: RoleAssistant}
	var text strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case anthropic.BlockText:
			text.WriteString(b.Text)
		case anthropic.BlockToolUse:
			args := string(b.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: FunctionCall{Name: b.Name, Arguments: args},
			})
		case anthropic.BlockThinking, anthropic.BlockRedactedThinking:
			// Thinking blocks are signed by the model that produced them and
			// cannot be replayed to a different provider. Dropping them is the
			// only correct option.
		}
	}
	if text.Len() > 0 {
		c, err := json.Marshal(text.String())
		if err != nil {
			return msg, false, err
		}
		msg.Content = c
	}
	if msg.Content == nil && len(msg.ToolCalls) == 0 {
		return msg, false, nil
	}
	return msg, true, nil
}

func userMessage(blocks []anthropic.ContentBlock) (ChatMsg, bool, error) {
	var parts []ContentPart
	for _, b := range blocks {
		switch b.Type {
		case anthropic.BlockText:
			if b.Text != "" {
				parts = append(parts, ContentPart{Type: "text", Text: b.Text})
			}
		case anthropic.BlockImage:
			if url := imageDataURL(b.Source); url != "" {
				parts = append(parts, ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: url}})
			}
		}
	}
	if len(parts) == 0 {
		return ChatMsg{}, false, nil
	}
	// Collapse the common all-text case to a plain string: some
	// OpenAI-compatible servers only accept the array form for real
	// multimodal input.
	if len(parts) == 1 && parts[0].Type == "text" {
		c, err := json.Marshal(parts[0].Text)
		if err != nil {
			return ChatMsg{}, false, err
		}
		return ChatMsg{Role: RoleUser, Content: c}, true, nil
	}
	c, err := json.Marshal(parts)
	if err != nil {
		return ChatMsg{}, false, err
	}
	return ChatMsg{Role: RoleUser, Content: c}, true, nil
}

func imageDataURL(s *anthropic.Source) string {
	if s == nil {
		return ""
	}
	switch s.Type {
	case "url":
		return s.URL
	case "base64":
		if s.MediaType == "" || s.Data == "" {
			return ""
		}
		return "data:" + s.MediaType + ";base64," + s.Data
	}
	return ""
}

// toolResultText flattens an Anthropic tool_result payload, which may be a
// bare string or an array of blocks, into the plain string OpenAI expects.
func toolResultText(b anthropic.ContentBlock) (string, error) {
	if len(b.Content) == 0 {
		return "", nil
	}
	blocks, err := anthropic.DecodeContent(b.Content)
	if err != nil {
		return "", fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
	}
	var sb strings.Builder
	for _, inner := range blocks {
		if inner.Type == anthropic.BlockText {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(inner.Text)
		}
	}
	return sb.String(), nil
}

func translateToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, fmt.Errorf("tool_choice: %w", err)
	}
	switch tc.Type {
	case "auto":
		return json.RawMessage(`"auto"`), nil
	case "any":
		return json.RawMessage(`"required"`), nil
	case "none":
		return json.RawMessage(`"none"`), nil
	case "tool":
		v, err := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		})
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, nil
}
