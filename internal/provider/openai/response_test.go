package openai

import (
	"encoding/json"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

func TestTranslateResponseText(t *testing.T) {
	in := &ChatResponse{
		ID:    "chatcmpl-1",
		Model: "gpt-5.6",
		Choices: []ChatChoice{{
			Message:      ChatMsg{Role: RoleAssistant, Content: json.RawMessage(`"hello there"`)},
			FinishReason: "stop",
		}},
		Usage: &ChatUsage{PromptTokens: 11, CompletionTokens: 3},
	}
	out, err := TranslateResponse(in, "anthropic/gpt-5.6", Options{})
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}
	if out.Model != "anthropic/gpt-5.6" {
		t.Errorf("Model = %q, want the gateway-facing ID", out.Model)
	}
	if out.Type != "message" || out.Role != anthropic.RoleAssistant {
		t.Errorf("envelope = %+v", out)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "hello there" {
		t.Fatalf("Content = %+v", out.Content)
	}
	if out.StopReason != anthropic.StopEndTurn {
		t.Errorf("StopReason = %q", out.StopReason)
	}
	if out.Usage.InputTokens != 11 || out.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v", out.Usage)
	}
}

func TestTranslateResponseToolCalls(t *testing.T) {
	in := &ChatResponse{
		Choices: []ChatChoice{{
			Message: ChatMsg{
				Role: RoleAssistant,
				ToolCalls: []ToolCall{{
					ID:       "call_1",
					Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	out, err := TranslateResponse(in, "m", Options{})
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}
	if len(out.Content) != 1 {
		t.Fatalf("Content = %+v", out.Content)
	}
	b := out.Content[0]
	if b.Type != anthropic.BlockToolUse || b.ID != "call_1" || b.Name != "get_weather" {
		t.Errorf("block = %+v", b)
	}
	if string(b.Input) != `{"city":"Paris"}` {
		t.Errorf("Input = %s", b.Input)
	}
	if out.StopReason != anthropic.StopToolUse {
		t.Errorf("StopReason = %q", out.StopReason)
	}
}

func TestTranslateResponseTruncatedToolArgumentsStayValidJSON(t *testing.T) {
	in := &ChatResponse{Choices: []ChatChoice{{
		Message: ChatMsg{ToolCalls: []ToolCall{{
			ID: "call_1", Function: FunctionCall{Name: "f", Arguments: `{"city":"Par`},
		}}},
		FinishReason: "length",
	}}}
	out, err := TranslateResponse(in, "m", Options{})
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}
	if !json.Valid(out.Content[0].Input) {
		t.Errorf("Input = %s, want valid JSON", out.Content[0].Input)
	}
}

func TestTranslateResponseReasoning(t *testing.T) {
	in := &ChatResponse{Choices: []ChatChoice{{
		Message: ChatMsg{
			ReasoningContent: "let me think",
			Content:          json.RawMessage(`"done"`),
		},
		FinishReason: "stop",
	}}}

	out, err := TranslateResponse(in, "m", Options{ReasoningMode: ReasoningAsThinking})
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}
	if len(out.Content) != 2 || out.Content[0].Type != anthropic.BlockThinking {
		t.Fatalf("Content = %+v", out.Content)
	}

	out, err = TranslateResponse(in, "m", Options{ReasoningMode: ReasoningDrop})
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}
	if len(out.Content) != 1 || out.Content[0].Type != anthropic.BlockText {
		t.Errorf("Content = %+v, want reasoning dropped", out.Content)
	}
}

func TestTranslateResponseCachedTokens(t *testing.T) {
	in := &ChatResponse{
		Choices: []ChatChoice{{FinishReason: "stop"}},
		Usage: &ChatUsage{
			PromptTokens:        100,
			CompletionTokens:    5,
			PromptTokensDetails: &PromptTokensDetails{CachedTokens: 80},
		},
	}
	out, err := TranslateResponse(in, "m", Options{})
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}
	// Anthropic counts cache reads separately from fresh input.
	if out.Usage.InputTokens != 20 || out.Usage.CacheReadInputTokens != 80 {
		t.Errorf("Usage = %+v, want 20 input / 80 cache read", out.Usage)
	}
}

func TestTranslateResponseNoChoices(t *testing.T) {
	out, err := TranslateResponse(&ChatResponse{}, "m", Options{})
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}
	if out.Content == nil {
		t.Error("Content must be an empty array, not null")
	}
	if out.StopReason != anthropic.StopEndTurn {
		t.Errorf("StopReason = %q", out.StopReason)
	}
}

func TestTranslateStopReason(t *testing.T) {
	tests := []struct {
		finish string
		tools  bool
		want   string
	}{
		{"stop", false, anthropic.StopEndTurn},
		{"length", false, anthropic.StopMaxTokens},
		{"tool_calls", true, anthropic.StopToolUse},
		{"function_call", true, anthropic.StopToolUse},
		{"content_filter", false, anthropic.StopEndTurn},
		{"", true, anthropic.StopToolUse},
		{"", false, anthropic.StopEndTurn},
	}
	for _, tc := range tests {
		if got := TranslateStopReason(tc.finish, tc.tools); got != tc.want {
			t.Errorf("TranslateStopReason(%q, %v) = %q, want %q", tc.finish, tc.tools, got, tc.want)
		}
	}
}

func TestMessageTextFromParts(t *testing.T) {
	got, err := messageText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`))
	if err != nil {
		t.Fatalf("messageText: %v", err)
	}
	if got != "ab" {
		t.Errorf("messageText = %q", got)
	}
}
