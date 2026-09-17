package openai

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

// frame is one decoded Anthropic SSE event.
type frame struct {
	name string
	data map[string]any
}

// runStream feeds an OpenAI SSE body through the translator and decodes the
// Anthropic stream it produces.
func runStream(t *testing.T, body string, opt Options) []frame {
	t.Helper()
	rec := httptest.NewRecorder()
	sw := anthropic.NewStreamWriter(rec)
	tr := NewStreamTranslator(sw, "anthropic/gpt-5.6", opt, 0)
	if err := tr.Run(strings.NewReader(body)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var out []frame
	r := anthropic.NewReader(rec.Body)
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading translated stream: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(f.Data), &m); err != nil {
			t.Fatalf("decoding %q: %v", f.Data, err)
		}
		out = append(out, frame{name: f.Name, data: m})
	}
	return out
}

func names(fs []frame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.name
	}
	return out
}

func joinText(fs []frame) string {
	var sb strings.Builder
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockDelta {
			continue
		}
		d, _ := f.data["delta"].(map[string]any)
		if d["type"] == anthropic.DeltaText {
			sb.WriteString(d["text"].(string))
		}
	}
	return sb.String()
}

func chunk(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(b) + "\n\n"
}

func TestStreamFinalUsage(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		usage                 string
		input, cached, output int
	}{
		{"uncached", `{"prompt_tokens":100,"completion_tokens":7}`, 100, 0, 7},
		{"cached", `{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":80}}`, 20, 80, 7},
		{"fully_cached", `{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":100}}`, 0, 100, 7},
		{"zero_cache", `{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":0}}`, 100, 0, 7},
		{"missing_usage", `null`, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("Hello")}}}}) +
				chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("stop")}}}) +
				"data: {\"choices\":[],\"usage\":" + tc.usage + "}\n\n" +
				"data: [DONE]\n\n"
			fs := runStream(t, body, Options{})
			if len(fs) < 2 || fs[len(fs)-2].name != anthropic.EvMessageDelta || fs[len(fs)-1].name != anthropic.EvMessageStop {
				t.Fatalf("missing terminal events: %v", names(fs))
			}
			usage := fs[len(fs)-2].data["usage"].(map[string]any)
			for field, want := range map[string]int{
				"input_tokens":                tc.input,
				"output_tokens":               tc.output,
				"cache_read_input_tokens":     tc.cached,
				"cache_creation_input_tokens": 0,
			} {
				if got := usage[field]; got != float64(want) {
					t.Errorf("%s = %v, want %d", field, got, want)
				}
			}
		})
	}
}

func TestStreamMessageStartCarriesTheEstimate(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := anthropic.NewStreamWriter(rec)
	tr := NewStreamTranslator(sw, "anthropic/gpt-5.6", Options{}, 4242)
	body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("Hi")}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("stop")}}})
	if err := tr.Run(strings.NewReader(body)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var out []frame
	r := anthropic.NewReader(rec.Body)
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading translated stream: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(f.Data), &m); err != nil {
			t.Fatalf("decoding %q: %v", f.Data, err)
		}
		out = append(out, frame{name: f.Name, data: m})
	}
	if len(out) == 0 || out[0].name != anthropic.EvMessageStart {
		t.Fatalf("expected message_start first, got %v", names(out))
	}
	// A translated backend only learns real usage once its stream completes,
	// so message_start would otherwise report zero — which Claude Code renders
	// as context usage flashing to zero every turn. See EstimateInputTokens.
	usage := out[0].data["message"].(map[string]any)["usage"].(map[string]any)
	if got := usage["input_tokens"]; got != float64(4242) {
		t.Errorf("message_start input_tokens = %v, want 4242 (the seeded estimate, not zero)", got)
	}
}

func TestStreamTextOnly(t *testing.T) {
	body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("Hel")}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("lo")}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("stop")}}}) +
		chunk(t, StreamChunk{Usage: &ChatUsage{PromptTokens: 5, CompletionTokens: 2}}) +
		"data: [DONE]\n\n"

	fs := runStream(t, body, Options{})
	want := []string{
		anthropic.EvMessageStart,
		anthropic.EvContentBlockStart,
		anthropic.EvContentBlockDelta,
		anthropic.EvContentBlockDelta,
		anthropic.EvContentBlockStop,
		anthropic.EvMessageDelta,
		anthropic.EvMessageStop,
	}
	if got := names(fs); !equal(got, want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}
	if got := joinText(fs); got != "Hello" {
		t.Errorf("text = %q, want Hello", got)
	}
	last := fs[len(fs)-2]
	delta := last.data["delta"].(map[string]any)
	if delta["stop_reason"] != anthropic.StopEndTurn {
		t.Errorf("stop_reason = %v, want end_turn", delta["stop_reason"])
	}
	usage := last.data["usage"].(map[string]any)
	if usage["output_tokens"].(float64) != 2 {
		t.Errorf("output_tokens = %v, want 2", usage["output_tokens"])
	}
}

func TestStreamModelIsTheIDClaudeCodePicked(t *testing.T) {
	body := chunk(t, StreamChunk{Model: "gpt-5.6", Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("x")}}}})
	fs := runStream(t, body, Options{})
	msg := fs[0].data["message"].(map[string]any)
	if msg["model"] != "anthropic/gpt-5.6" {
		t.Errorf("model = %v, want the gateway-facing ID", msg["model"])
	}
}

func TestStreamToolCall(t *testing.T) {
	body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{
		ToolCalls: []ToolCall{{Index: 0, ID: "call_1", Type: "function",
			Function: FunctionCall{Name: "get_weather", Arguments: `{"ci`}}},
	}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{
			ToolCalls: []ToolCall{{Index: 0, Function: FunctionCall{Arguments: `ty":"Paris"}`}}},
		}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("tool_calls")}}}) +
		"data: [DONE]\n\n"

	fs := runStream(t, body, Options{})
	start := findFrame(t, fs, anthropic.EvContentBlockStart)
	block := start.data["content_block"].(map[string]any)
	if block["type"] != anthropic.BlockToolUse {
		t.Fatalf("block type = %v, want tool_use", block["type"])
	}
	if block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Errorf("block = %+v", block)
	}

	var args strings.Builder
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockDelta {
			continue
		}
		d := f.data["delta"].(map[string]any)
		if d["type"] == anthropic.DeltaInputJSON {
			args.WriteString(d["partial_json"].(string))
		}
	}
	if args.String() != `{"city":"Paris"}` {
		t.Errorf("reassembled arguments = %q", args.String())
	}

	md := findFrame(t, fs, anthropic.EvMessageDelta)
	if md.data["delta"].(map[string]any)["stop_reason"] != anthropic.StopToolUse {
		t.Errorf("stop_reason = %v, want tool_use", md.data["delta"])
	}
}

func TestStreamTextThenToolUsesSeparateBlockIndices(t *testing.T) {
	body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("thinking...")}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{
			ToolCalls: []ToolCall{{Index: 0, ID: "call_1", Function: FunctionCall{Name: "f", Arguments: "{}"}}},
		}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("tool_calls")}}})

	fs := runStream(t, body, Options{})
	var starts []float64
	for _, f := range fs {
		if f.name == anthropic.EvContentBlockStart {
			starts = append(starts, f.data["index"].(float64))
		}
	}
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 1 {
		t.Fatalf("content_block_start indices = %v, want [0 1]", starts)
	}
	// Every opened block must be closed, or Claude Code's parser is left
	// holding an unterminated block.
	var stops int
	for _, f := range fs {
		if f.name == anthropic.EvContentBlockStop {
			stops++
		}
	}
	if stops != 2 {
		t.Errorf("content_block_stop count = %d, want 2", stops)
	}
}

func TestStreamTwoToolCallsGetDistinctBlocks(t *testing.T) {
	body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{
		ToolCalls: []ToolCall{
			{Index: 0, ID: "call_a", Function: FunctionCall{Name: "a", Arguments: `{"x":1}`}},
			{Index: 1, ID: "call_b", Function: FunctionCall{Name: "b", Arguments: `{"y":2}`}},
		},
	}}}}) + chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("tool_calls")}}})

	fs := runStream(t, body, Options{})
	ids := map[string]float64{}
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockStart {
			continue
		}
		b := f.data["content_block"].(map[string]any)
		ids[b["id"].(string)] = f.data["index"].(float64)
	}
	if len(ids) != 2 || ids["call_a"] == ids["call_b"] {
		t.Fatalf("tool blocks = %v, want two distinct indices", ids)
	}
}

func TestStreamInterleavedToolArgumentsGoToTheRightBlock(t *testing.T) {
	// Argument fragments for tool 0 arriving after tool 1 has opened must be
	// addressed by the recorded block index, not the currently open one.
	body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{
		ToolCalls: []ToolCall{{Index: 0, ID: "call_a", Function: FunctionCall{Name: "a"}}},
	}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{
			ToolCalls: []ToolCall{{Index: 1, ID: "call_b", Function: FunctionCall{Name: "b"}}},
		}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{
			ToolCalls: []ToolCall{{Index: 0, Function: FunctionCall{Arguments: `{"late":true}`}}},
		}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("tool_calls")}}})

	fs := runStream(t, body, Options{})
	blockOf := map[string]float64{}
	for _, f := range fs {
		if f.name == anthropic.EvContentBlockStart {
			b := f.data["content_block"].(map[string]any)
			blockOf[b["id"].(string)] = f.data["index"].(float64)
		}
	}
	var argIndex float64 = -1
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockDelta {
			continue
		}
		if d := f.data["delta"].(map[string]any); d["type"] == anthropic.DeltaInputJSON {
			argIndex = f.data["index"].(float64)
		}
	}
	if argIndex != blockOf["call_a"] {
		t.Errorf("late arguments went to block %v, want call_a's block %v", argIndex, blockOf["call_a"])
	}
}

func TestStreamReasoningBecomesThinking(t *testing.T) {
	body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{ReasoningContent: ptr("pondering")}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("answer")}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("stop")}}})

	fs := runStream(t, body, Options{ReasoningMode: ReasoningAsThinking})
	start := findFrame(t, fs, anthropic.EvContentBlockStart)
	if start.data["content_block"].(map[string]any)["type"] != anthropic.BlockThinking {
		t.Fatalf("first block = %v, want thinking", start.data["content_block"])
	}
	if got := joinText(fs); got != "answer" {
		t.Errorf("text = %q, want answer", got)
	}
}

func TestStreamReasoningDropped(t *testing.T) {
	body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{ReasoningContent: ptr("pondering")}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("answer")}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("stop")}}})

	fs := runStream(t, body, Options{ReasoningMode: ReasoningDrop})
	for _, f := range fs {
		if f.name == anthropic.EvContentBlockStart {
			if f.data["content_block"].(map[string]any)["type"] == anthropic.BlockThinking {
				t.Fatal("thinking block emitted although reasoning is set to drop")
			}
		}
	}
}

func TestStreamUpstreamErrorBecomesErrorEvent(t *testing.T) {
	body := chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("partial")}}}}) +
		`data: {"error":{"message":"upstream exploded","type":"server_error"}}` + "\n\n"

	fs := runStream(t, body, Options{})
	last := fs[len(fs)-1]
	if last.name != anthropic.EvError {
		t.Fatalf("last event = %q, want error", last.name)
	}
	e := last.data["error"].(map[string]any)
	if e["message"] != "upstream exploded" {
		t.Errorf("error message = %v", e["message"])
	}
}

func TestStreamEmptyUpstreamStillTerminates(t *testing.T) {
	// Claude Code waits for message_stop; a stream that ends without one
	// leaves the UI hanging.
	fs := runStream(t, "data: [DONE]\n\n", Options{})
	if got := names(fs); !equal(got, []string{
		anthropic.EvMessageStart, anthropic.EvMessageDelta, anthropic.EvMessageStop,
	}) {
		t.Errorf("sequence = %v", got)
	}
}

func TestStreamIgnoresUnparseableFrames(t *testing.T) {
	body := "data: {not json\n\n" +
		chunk(t, StreamChunk{Choices: []StreamChoice{{Delta: ChunkDelta{Content: ptr("ok")}}}}) +
		chunk(t, StreamChunk{Choices: []StreamChoice{{FinishReason: ptr("stop")}}})
	fs := runStream(t, body, Options{})
	if got := joinText(fs); got != "ok" {
		t.Errorf("text = %q, want ok", got)
	}
}

func findFrame(t *testing.T, fs []frame, name string) frame {
	t.Helper()
	for _, f := range fs {
		if f.name == name {
			return f
		}
	}
	t.Fatalf("no %s event in %v", name, names(fs))
	return frame{}
}

func ptr[T any](v T) *T { return &v }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
