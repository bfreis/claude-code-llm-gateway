package codex

import (
	"encoding/json"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTranslateRequestBasics(t *testing.T) {
	in := &anthropic.MessagesRequest{
		System: mustJSON(t, "be brief"),
		Messages: []anthropic.Message{
			{Role: anthropic.RoleUser, Content: mustJSON(t, "hello")},
		},
	}
	out, err := TranslateRequest(in, "gpt-5.6-sol", Options{})
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if out.Model != "gpt-5.6-sol" {
		t.Errorf("Model = %q", out.Model)
	}
	// The Responses API has no system message; the system prompt is a
	// top-level field.
	if out.Instructions != "be brief" {
		t.Errorf("Instructions = %q", out.Instructions)
	}
	if !out.Stream || out.Store {
		t.Errorf("stream=%v store=%v, want stream only", out.Stream, out.Store)
	}
	if len(out.Include) != 1 || out.Include[0] != IncludeEncryptedReasoning {
		t.Errorf("Include = %v", out.Include)
	}
	if len(out.Input) != 1 {
		t.Fatalf("Input = %+v", out.Input)
	}
	got := out.Input[0]
	if got.Type != ItemMessage || got.Role != RoleUser {
		t.Errorf("item = %+v", got)
	}
	if len(got.Content) != 1 || got.Content[0].Type != PartInputText || got.Content[0].Text != "hello" {
		t.Errorf("content = %+v", got.Content)
	}
}

func TestToolsAreFlatWithNoFunctionWrapper(t *testing.T) {
	in := &anthropic.MessagesRequest{Tools: []anthropic.Tool{{
		Name:        "Read",
		Description: "read a file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}}}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("Tools = %+v", out.Tools)
	}

	// The wire shape is {"type":"function","name":...}, NOT
	// {"type":"function","function":{...}} as Chat Completions uses.
	encoded, err := json.Marshal(out.Tools[0])
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(encoded, &probe); err != nil {
		t.Fatal(err)
	}
	if _, nested := probe["function"]; nested {
		t.Errorf("tool must not be nested under `function`: %s", encoded)
	}
	if probe["type"] != "function" || probe["name"] != "Read" {
		t.Errorf("tool = %s", encoded)
	}
	if probe["strict"] != false {
		t.Errorf("strict = %v, want false: strict mode rejects schemas Claude Code sends", probe["strict"])
	}
}

func TestToolCallRoundTripBecomesPeerItems(t *testing.T) {
	// Anthropic nests a tool call inside an assistant message and its result
	// inside a user message. The Responses API wants them as flat peer items.
	in := &anthropic.MessagesRequest{
		Messages: []anthropic.Message{
			{Role: anthropic.RoleUser, Content: mustJSON(t, "list the repo")},
			{Role: anthropic.RoleAssistant, Content: mustJSON(t, []anthropic.ContentBlock{
				{Type: anthropic.BlockText, Text: "checking"},
				{Type: anthropic.BlockToolUse, ID: "call_1", Name: "Bash",
					Input: json.RawMessage(`{"command":"ls"}`)},
			})},
			{Role: anthropic.RoleUser, Content: mustJSON(t, []anthropic.ContentBlock{
				{Type: anthropic.BlockToolResult, ToolUseID: "call_1", Content: mustJSON(t, "README.md")},
			})},
		},
	}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}

	wantTypes := []string{ItemMessage, ItemMessage, ItemFunctionCall, ItemFunctionCallOutput}
	if len(out.Input) != len(wantTypes) {
		t.Fatalf("got %d items, want %d: %+v", len(out.Input), len(wantTypes), out.Input)
	}
	for i, want := range wantTypes {
		if out.Input[i].Type != want {
			t.Errorf("Input[%d].Type = %q, want %q", i, out.Input[i].Type, want)
		}
	}

	call := out.Input[2]
	if call.CallID != "call_1" || call.Name != "Bash" {
		t.Errorf("function_call = %+v", call)
	}
	// arguments is a JSON-encoded string, not an object.
	if call.Arguments != `{"command":"ls"}` {
		t.Errorf("Arguments = %q, want the raw JSON string", call.Arguments)
	}
	encoded, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(encoded, &probe); err != nil {
		t.Fatal(err)
	}
	if _, isString := probe["arguments"].(string); !isString {
		t.Errorf("arguments must serialise as a string, got %T: %s", probe["arguments"], encoded)
	}

	result := out.Input[3]
	if result.CallID != "call_1" {
		t.Errorf("function_call_output.call_id = %q", result.CallID)
	}
	// output is a bare JSON string for a text result.
	if string(result.Output) != `"README.md"` {
		t.Errorf("output = %s, want a bare JSON string", result.Output)
	}
}

func TestAssistantTextBecomesOutputText(t *testing.T) {
	in := &anthropic.MessagesRequest{Messages: []anthropic.Message{
		{Role: anthropic.RoleAssistant, Content: mustJSON(t, "prior answer")},
	}}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Input[0]
	if got.Role != RoleAssistant || got.Content[0].Type != PartOutputText {
		t.Errorf("assistant item = %+v", got)
	}
}

func TestThinkingBlocksAreNotReplayed(t *testing.T) {
	// Replaying reasoning without the backend's encrypted blob is meaningless,
	// and Claude Code never carries that blob back to us.
	in := &anthropic.MessagesRequest{Messages: []anthropic.Message{
		{Role: anthropic.RoleAssistant, Content: mustJSON(t, []anthropic.ContentBlock{
			{Type: anthropic.BlockThinking, Thinking: "secret"},
			{Type: anthropic.BlockText, Text: "answer"},
		})},
	}}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range out.Input {
		if item.Type == ItemReasoning {
			t.Error("a reasoning item was replayed")
		}
	}
	if out.Input[0].Content[0].Text != "answer" {
		t.Errorf("text = %q", out.Input[0].Content[0].Text)
	}
}

func TestPlanToolsWithheldByDefault(t *testing.T) {
	in := &anthropic.MessagesRequest{Tools: []anthropic.Tool{
		{Name: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "ExitPlanMode", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	out, err := TranslateRequest(in, "m", Options{DropPlanTools: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "Read" {
		t.Errorf("tools = %+v, want only Read", out.Tools)
	}
}

func TestThinkingBudgetBecomesReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		budget int
		want   string
	}{{2048, "low"}, {10000, "medium"}, {32000, "high"}} {
		in := &anthropic.MessagesRequest{
			Thinking: &anthropic.Thinking{Type: "enabled", BudgetTokens: tc.budget},
		}
		out, err := TranslateRequest(in, "m", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Reasoning == nil || out.Reasoning.Effort != tc.want {
			t.Errorf("budget %d -> %+v, want effort %q", tc.budget, out.Reasoning, tc.want)
		}
	}

	// Thinking off means no reasoning block at all.
	out, err := TranslateRequest(&anthropic.MessagesRequest{}, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reasoning != nil {
		t.Errorf("Reasoning = %+v, want nil when thinking is off", out.Reasoning)
	}
}

func TestSystemBlocksAreJoined(t *testing.T) {
	in := &anthropic.MessagesRequest{System: mustJSON(t, []anthropic.ContentBlock{
		{Type: anthropic.BlockText, Text: "first"},
		{Type: anthropic.BlockText, Text: "second"},
	})}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Instructions != "first\n\nsecond" {
		t.Errorf("Instructions = %q", out.Instructions)
	}
}

func TestImagesBecomeInputImage(t *testing.T) {
	in := &anthropic.MessagesRequest{Messages: []anthropic.Message{{
		Role: anthropic.RoleUser,
		Content: mustJSON(t, []anthropic.ContentBlock{
			{Type: anthropic.BlockText, Text: "what is this"},
			{Type: anthropic.BlockImage, Source: &anthropic.Source{
				Type: "base64", MediaType: "image/png", Data: "AAAA"}},
		}),
	}}}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	parts := out.Input[0].Content
	if len(parts) != 2 || parts[1].Type != PartInputImage {
		t.Fatalf("parts = %+v", parts)
	}
	if parts[1].ImageURL != "data:image/png;base64,AAAA" {
		t.Errorf("image url = %q", parts[1].ImageURL)
	}
}

func TestToolChoiceMapping(t *testing.T) {
	for in, want := range map[string]string{
		`{"type":"auto"}`:               "auto",
		`{"type":"any"}`:                "required",
		`{"type":"none"}`:               "none",
		`{"type":"tool","name":"Bash"}`: "required",
	} {
		out, err := TranslateRequest(
			&anthropic.MessagesRequest{ToolChoice: json.RawMessage(in)}, "m", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if out.ToolChoice != want {
			t.Errorf("tool_choice %s -> %q, want %q", in, out.ToolChoice, want)
		}
	}

	// Absent tool_choice must still send a valid value: the field is not
	// optional on this API.
	out, err := TranslateRequest(&anthropic.MessagesRequest{}, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolChoice != "auto" {
		t.Errorf("default tool_choice = %q, want auto", out.ToolChoice)
	}
}

func TestEmptyInputStillSerialisesAsAnArray(t *testing.T) {
	out, err := TranslateRequest(&anthropic.MessagesRequest{}, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) {
		t.Fatal("invalid JSON")
	}
	var probe map[string]any
	if err := json.Unmarshal(encoded, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["input"].([]any); !ok {
		t.Errorf("input = %v, want an array rather than null", probe["input"])
	}
}

func TestToolResultPrecedesRemainingUserContent(t *testing.T) {
	// A single Anthropic user message can carry both a tool result and new
	// text. The function_call_output has to stay adjacent to the function_call
	// it answers, so it must be emitted before the user's other content.
	in := &anthropic.MessagesRequest{Messages: []anthropic.Message{
		{Role: anthropic.RoleAssistant, Content: mustJSON(t, []anthropic.ContentBlock{
			{Type: anthropic.BlockToolUse, ID: "call_1", Name: "Bash",
				Input: json.RawMessage(`{"command":"ls"}`)},
		})},
		{Role: anthropic.RoleUser, Content: mustJSON(t, []anthropic.ContentBlock{
			{Type: anthropic.BlockText, Text: "and also do this"},
			{Type: anthropic.BlockToolResult, ToolUseID: "call_1", Content: mustJSON(t, "README.md")},
		})},
	}}
	out, err := TranslateRequest(in, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{ItemFunctionCall, ItemFunctionCallOutput, ItemMessage}
	if len(out.Input) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(out.Input), len(want), out.Input)
	}
	for i, w := range want {
		if out.Input[i].Type != w {
			t.Errorf("Input[%d].Type = %q, want %q — the tool result must follow its call",
				i, out.Input[i].Type, w)
		}
	}
}
