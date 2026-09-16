package openai

import (
	"encoding/json"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestTranslateRequestBasics(t *testing.T) {
	temp := 0.7
	in := &anthropic.MessagesRequest{
		Model:         "ignored",
		System:        mustJSON(t, "be brief"),
		MaxTokens:     1000,
		Temperature:   &temp,
		StopSequences: []string{"STOP"},
		Stream:        true,
		Messages: []anthropic.Message{
			{Role: anthropic.RoleUser, Content: mustJSON(t, "hello")},
		},
	}
	out, err := TranslateRequest(in, "gpt-5.6", Options{UseMaxCompletionTokens: true})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if out.Model != "gpt-5.6" {
		t.Errorf("Model = %q", out.Model)
	}
	if out.MaxCompletionTokens != 1000 || out.MaxTokens != 0 {
		t.Errorf("max tokens routed wrong: completion=%d legacy=%d", out.MaxCompletionTokens, out.MaxTokens)
	}
	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Error("stream requests must ask for usage")
	}
	if len(out.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want system + user", len(out.Messages))
	}
	if out.Messages[0].Role != RoleSystem || string(out.Messages[0].Content) != `"be brief"` {
		t.Errorf("system message = %+v", out.Messages[0])
	}
	if out.Messages[1].Role != RoleUser || string(out.Messages[1].Content) != `"hello"` {
		t.Errorf("user message = %+v", out.Messages[1])
	}
	if out.Temperature == nil || *out.Temperature != 0.7 {
		t.Errorf("Temperature = %v", out.Temperature)
	}
}

func TestTranslateRequestLegacyMaxTokens(t *testing.T) {
	in := &anthropic.MessagesRequest{MaxTokens: 512}
	out, err := TranslateRequest(in, "m", Options{UseMaxCompletionTokens: false})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if out.MaxTokens != 512 || out.MaxCompletionTokens != 0 {
		t.Errorf("completion=%d legacy=%d", out.MaxCompletionTokens, out.MaxTokens)
	}
}

func TestTranslateRequestDropsTemperature(t *testing.T) {
	temp := 0.5
	in := &anthropic.MessagesRequest{Temperature: &temp, TopP: &temp}
	out, err := TranslateRequest(in, "m", Options{DropTemperature: true})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if out.Temperature != nil || out.TopP != nil {
		t.Errorf("temperature/top_p should be omitted, got %v / %v", out.Temperature, out.TopP)
	}
}

func TestTranslateRequestSystemBlocks(t *testing.T) {
	in := &anthropic.MessagesRequest{
		System: mustJSON(t, []anthropic.ContentBlock{
			{Type: anthropic.BlockText, Text: "first"},
			{Type: anthropic.BlockText, Text: "second"},
		}),
	}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("len(Messages) = %d", len(out.Messages))
	}
	var got string
	if err := json.Unmarshal(out.Messages[0].Content, &got); err != nil {
		t.Fatal(err)
	}
	if got != "first\n\nsecond" {
		t.Errorf("system content = %q", got)
	}
}

func TestTranslateRequestToolRoundTrip(t *testing.T) {
	in := &anthropic.MessagesRequest{
		Tools: []anthropic.Tool{{
			Name:        "get_weather",
			Description: "look up weather",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		ToolChoice: json.RawMessage(`{"type":"any"}`),
		Messages: []anthropic.Message{
			{Role: anthropic.RoleUser, Content: mustJSON(t, "weather?")},
			{Role: anthropic.RoleAssistant, Content: mustJSON(t, []anthropic.ContentBlock{
				{Type: anthropic.BlockText, Text: "checking"},
				{Type: anthropic.BlockToolUse, ID: "toolu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Paris"}`)},
			})},
			{Role: anthropic.RoleUser, Content: mustJSON(t, []anthropic.ContentBlock{
				{Type: anthropic.BlockToolResult, ToolUseID: "toolu_1", Content: mustJSON(t, "18C")},
			})},
		},
	}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}

	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("Tools = %+v", out.Tools)
	}
	if string(out.ToolChoice) != `"required"` {
		t.Errorf("ToolChoice = %s, want \"required\"", out.ToolChoice)
	}

	// user, assistant(+tool_calls), tool
	if len(out.Messages) != 3 {
		t.Fatalf("len(Messages) = %d: %+v", len(out.Messages), out.Messages)
	}
	asst := out.Messages[1]
	if asst.Role != RoleAssistant || len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant = %+v", asst)
	}
	if asst.ToolCalls[0].ID != "toolu_1" || asst.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool call = %+v", asst.ToolCalls[0])
	}
	toolMsg := out.Messages[2]
	if toolMsg.Role != RoleTool || toolMsg.ToolCallID != "toolu_1" {
		t.Fatalf("tool message = %+v", toolMsg)
	}
	if string(toolMsg.Content) != `"18C"` {
		t.Errorf("tool content = %s", toolMsg.Content)
	}
}

func TestToolResultPrecedesRemainingUserContent(t *testing.T) {
	// A single Anthropic user message can hold both a tool result and new
	// text. OpenAI needs the tool message first so it answers the preceding
	// assistant tool_calls.
	in := &anthropic.MessagesRequest{
		Messages: []anthropic.Message{{
			Role: anthropic.RoleUser,
			Content: mustJSON(t, []anthropic.ContentBlock{
				{Type: anthropic.BlockText, Text: "and also this"},
				{Type: anthropic.BlockToolResult, ToolUseID: "toolu_9", Content: mustJSON(t, "result")},
			}),
		}},
	}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("len(Messages) = %d", len(out.Messages))
	}
	if out.Messages[0].Role != RoleTool {
		t.Errorf("Messages[0].Role = %q, want the tool result first", out.Messages[0].Role)
	}
	if out.Messages[1].Role != RoleUser {
		t.Errorf("Messages[1].Role = %q, want user", out.Messages[1].Role)
	}
}

func TestTranslateRequestImage(t *testing.T) {
	in := &anthropic.MessagesRequest{
		Messages: []anthropic.Message{{
			Role: anthropic.RoleUser,
			Content: mustJSON(t, []anthropic.ContentBlock{
				{Type: anthropic.BlockText, Text: "what is this"},
				{Type: anthropic.BlockImage, Source: &anthropic.Source{
					Type: "base64", MediaType: "image/png", Data: "AAAA",
				}},
			}),
		}},
	}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	var parts []ContentPart
	if err := json.Unmarshal(out.Messages[0].Content, &parts); err != nil {
		t.Fatalf("multimodal content should be an array: %v", err)
	}
	if len(parts) != 2 || parts[1].ImageURL == nil {
		t.Fatalf("parts = %+v", parts)
	}
	if parts[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Errorf("image url = %q", parts[1].ImageURL.URL)
	}
}

func TestTranslateRequestDropsThinkingBlocks(t *testing.T) {
	in := &anthropic.MessagesRequest{
		Messages: []anthropic.Message{{
			Role: anthropic.RoleAssistant,
			Content: mustJSON(t, []anthropic.ContentBlock{
				{Type: anthropic.BlockThinking, Thinking: "secret", Signature: "sig"},
				{Type: anthropic.BlockText, Text: "answer"},
			}),
		}},
	}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if got := string(out.Messages[0].Content); got != `"answer"` {
		t.Errorf("content = %s, want the thinking block dropped", got)
	}
}

func TestThinkingBudgetBecomesReasoningEffort(t *testing.T) {
	tests := []struct {
		budget int
		want   string
	}{
		{0, ""},
		{2048, "low"},
		{10000, "medium"},
		{32000, "high"},
	}
	for _, tc := range tests {
		in := &anthropic.MessagesRequest{Thinking: &anthropic.Thinking{Type: "enabled", BudgetTokens: tc.budget}}
		out, err := TranslateRequest(in, "m", Options{})
		if err != nil {
			t.Fatalf("TranslateRequest: %v", err)
		}
		if out.ReasoningEffort != tc.want {
			t.Errorf("budget %d -> %q, want %q", tc.budget, out.ReasoningEffort, tc.want)
		}
	}
}

func TestMaxTokensCap(t *testing.T) {
	in := &anthropic.MessagesRequest{MaxTokens: 100000}
	out, err := TranslateRequest(in, "m", Options{MaxTokensCap: 4096})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if out.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want the cap", out.MaxTokens)
	}
}

func TestToolChoiceMappings(t *testing.T) {
	tests := map[string]string{
		`{"type":"auto"}`:                      `"auto"`,
		`{"type":"any"}`:                       `"required"`,
		`{"type":"none"}`:                      `"none"`,
		`{"type":"tool","name":"get_weather"}`: `{"function":{"name":"get_weather"},"type":"function"}`,
	}
	for in, want := range tests {
		got, err := translateToolChoice(json.RawMessage(in))
		if err != nil {
			t.Fatalf("translateToolChoice(%s): %v", in, err)
		}
		if string(got) != want {
			t.Errorf("translateToolChoice(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestPlanToolsAreWithheldByDefault(t *testing.T) {
	// A non-Claude model handed EnterPlanMode/ExitPlanMode calls them
	// unprompted, which stalls the turn.
	in := &anthropic.MessagesRequest{Tools: []anthropic.Tool{
		{Name: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "EnterPlanMode", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "ExitPlanMode", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	out, err := TranslateRequest(in, "m", Options{DropPlanTools: true})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "Read" {
		names := make([]string, len(out.Tools))
		for i, tool := range out.Tools {
			names[i] = tool.Function.Name
		}
		t.Errorf("tools = %v, want only Read", names)
	}
}

func TestPlanToolsCanBeKept(t *testing.T) {
	in := &anthropic.MessagesRequest{Tools: []anthropic.Tool{
		{Name: "ExitPlanMode", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	out, err := TranslateRequest(in, "m", Options{DropPlanTools: false})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Errorf("tools = %v, want the plan tool kept", out.Tools)
	}
}
