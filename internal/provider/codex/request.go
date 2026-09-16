package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

// Options tune the translation for one configured model.
type Options struct {
	// DropPlanTools withholds Claude Code's plan-mode tools.
	DropPlanTools bool
	// ServiceTier is passed through when set ("priority" for the -fast forms).
	ServiceTier string
	// ReasoningSummary asks for a reasoning summary ("auto", "concise",
	// "detailed"); empty leaves the backend's default.
	ReasoningSummary string
}

// PlanTools are Claude Code's plan-mode tools, withheld by default because a
// non-Claude model calls them unprompted and stalls the turn.
var PlanTools = map[string]bool{
	"EnterPlanMode": true,
	"ExitPlanMode":  true,
}

// TranslateRequest converts an Anthropic Messages request into a Codex
// Responses request.
//
// Two shape differences drive most of this. Anthropic nests tool calls and
// their results inside assistant and user *messages*; the Responses API makes
// them peer items in a flat `input` list. And Anthropic carries a separate
// `system` field, where the Responses API has `instructions`.
func TranslateRequest(in *anthropic.MessagesRequest, model string, opt Options) (*Request, error) {
	out := &Request{
		Model:             model,
		Input:             []Item{},
		ToolChoice:        "auto",
		ParallelToolCalls: false,
		Store:             false,
		Stream:            true,
		// Asking for encrypted reasoning costs nothing when unused and is what
		// the Codex CLI sends unconditionally.
		Include:     []string{IncludeEncryptedReasoning},
		ServiceTier: opt.ServiceTier,
	}

	instructions, err := systemText(in.System)
	if err != nil {
		return nil, err
	}
	out.Instructions = instructions

	if in.Thinking.Enabled() {
		out.Reasoning = &Reasoning{
			Effort:  effortFor(in.Thinking.BudgetTokens),
			Summary: opt.ReasoningSummary,
		}
	}

	for i, m := range in.Messages {
		items, err := translateMessage(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Input = append(out.Input, items...)
	}

	for _, t := range in.Tools {
		// Server-side tool types carry no schema and have no equivalent here.
		if t.Type != "" && t.InputSchema == nil {
			continue
		}
		if opt.DropPlanTools && PlanTools[t.Name] {
			continue
		}
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, Tool{
			Type:        ToolTypeFunction,
			Name:        t.Name,
			Description: t.Description,
			// The Codex CLI hardcodes strict:false for every converted tool;
			// strict mode would reject schemas Claude Code routinely sends.
			Strict:     false,
			Parameters: params,
		})
	}

	if choice := translateToolChoice(in.ToolChoice); choice != "" {
		out.ToolChoice = choice
	}
	return out, nil
}

// effortFor maps an Anthropic thinking budget onto the coarse effort knob.
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

// systemText flattens Anthropic's string-or-blocks system field.
func systemText(raw json.RawMessage) (string, error) {
	blocks, err := anthropic.DecodeContent(raw)
	if err != nil {
		return "", fmt.Errorf("system: %w", err)
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
	return sb.String(), nil
}

// translateMessage converts one Anthropic message into Responses input items.
//
// One message can yield several items: an assistant turn holding text plus two
// tool calls becomes a message item and two function_call items.
func translateMessage(m anthropic.Message) ([]Item, error) {
	blocks, err := m.Blocks()
	if err != nil {
		return nil, err
	}

	var items []Item

	// Tool results come first so they follow the function_call they answer.
	for _, b := range blocks {
		if b.Type != anthropic.BlockToolResult {
			continue
		}
		text, err := toolResultText(b)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		items = append(items, Item{
			Type:   ItemFunctionCallOutput,
			CallID: b.ToolUseID,
			Output: encoded,
		})
	}

	if m.Role == anthropic.RoleAssistant {
		return append(items, assistantItems(blocks)...), nil
	}
	if part, ok := userMessage(blocks); ok {
		items = append(items, part)
	}
	return items, nil
}

func assistantItems(blocks []anthropic.ContentBlock) []Item {
	var items []Item
	// Reasoning items precede the content they produced, matching the order the
	// backend emitted them in.
	var reasoning []Item
	var text strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case anthropic.BlockText:
			text.WriteString(b.Text)
		case anthropic.BlockThinking:
			// A thinking block this gateway produced carries the backend's
			// encrypted reasoning in its signature. Replaying it preserves the
			// model's chain of thought across turns without any server-side
			// session state. Anything else — Anthropic's own signed blocks, or
			// an unsigned one — has nothing this backend can use.
			if id, encrypted, ok := DecodeReasoning(b.Signature); ok {
				reasoning = append(reasoning, Item{
					Type:             ItemReasoning,
					ID:               id,
					EncryptedContent: encrypted,
				})
			}
		case anthropic.BlockRedactedThinking:
		}
	}
	items = append(items, reasoning...)
	if text.Len() > 0 {
		items = append(items, Item{
			Type:    ItemMessage,
			Role:    RoleAssistant,
			Content: []Part{{Type: PartOutputText, Text: text.String()}},
		})
	}
	for _, b := range blocks {
		if b.Type != anthropic.BlockToolUse {
			continue
		}
		args := string(b.Input)
		if args == "" || args == "null" {
			args = "{}"
		}
		items = append(items, Item{
			Type:      ItemFunctionCall,
			CallID:    b.ID,
			Name:      b.Name,
			Arguments: args,
		})
	}
	return items
}

func userMessage(blocks []anthropic.ContentBlock) (Item, bool) {
	var parts []Part
	for _, b := range blocks {
		switch b.Type {
		case anthropic.BlockText:
			if b.Text != "" {
				parts = append(parts, Part{Type: PartInputText, Text: b.Text})
			}
		case anthropic.BlockImage:
			if url := imageDataURL(b.Source); url != "" {
				parts = append(parts, Part{Type: PartInputImage, ImageURL: url})
			}
		}
	}
	if len(parts) == 0 {
		return Item{}, false
	}
	return Item{Type: ItemMessage, Role: RoleUser, Content: parts}, true
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

// toolResultText flattens an Anthropic tool_result payload into the plain
// string the Responses API expects for `output`.
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

// translateToolChoice maps Anthropic's tool_choice onto the Responses API's.
func translateToolChoice(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var tc struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return ""
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		// Naming a single required tool has no direct equivalent here; "required"
		// keeps the caller's intent that *some* tool must be used.
		return "required"
	}
	return ""
}
