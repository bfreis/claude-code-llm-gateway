package codex

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

type frame struct {
	name string
	data map[string]any
}

// sse renders a Codex Responses SSE frame: `event: {type}` then the JSON.
func sse(t *testing.T, ev map[string]any) string {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return "event: " + ev["type"].(string) + "\ndata: " + string(b) + "\n\n"
}

func runStream(t *testing.T, body string, opt StreamOptions) []frame {
	t.Helper()
	return runStreamWithEstimate(t, body, opt, 0)
}

func runStreamWithEstimate(t *testing.T, body string, opt StreamOptions, estimate int) []frame {
	t.Helper()
	rec := httptest.NewRecorder()
	sw := anthropic.NewStreamWriter(rec)
	tr := NewStreamTranslator(sw, "anthropic/gpt-5.6-sol", opt, estimate)
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

func TestStreamMessageStartCarriesTheEstimate(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := anthropic.NewStreamWriter(rec)
	tr := NewStreamTranslator(sw, "anthropic/gpt-5.6-sol", StreamOptions{}, 4242)
	body := sse(t, map[string]any{"type": EvOutputTextDelta, "delta": "Hi"}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"usage": nil}})
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
	// Codex only learns real usage once its stream completes, so message_start
	// would otherwise report zero — which Claude Code renders as context usage
	// flashing to zero every turn. See anthropic.EstimateInputTokens.
	usage := out[0].data["message"].(map[string]any)["usage"].(map[string]any)
	if got := usage["input_tokens"]; got != float64(4242) {
		t.Errorf("message_start input_tokens = %v, want 4242 (the seeded estimate, not zero)", got)
	}
}

func TestStreamFinalUsage(t *testing.T) {
	for _, terminal := range []string{EvCompleted, EvIncomplete} {
		for _, tc := range []struct {
			name                  string
			usage                 string
			input, cached, output int
		}{
			{"uncached", `{"input_tokens":100,"output_tokens":7}`, 100, 0, 7},
			{"cached", `{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80}}`, 20, 80, 7},
			{"fully_cached", `{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":100}}`, 0, 100, 7},
			{"zero_cache", `{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":0}}`, 100, 0, 7},
			// Zero only because runStream seeds no estimate; with one, finish
			// falls back to it. See TestStreamFinalUsageFallsBackToTheEstimate.
			{"missing_usage", `null`, 0, 0, 0},
		} {
			t.Run(terminal+"/"+tc.name, func(t *testing.T) {
				body := sse(t, map[string]any{"type": EvOutputTextDelta, "delta": "Hello"}) +
					sse(t, map[string]any{"type": terminal, "response": map[string]any{"usage": json.RawMessage(tc.usage)}})
				fs := runStream(t, body, StreamOptions{})
				if len(fs) < 2 || fs[len(fs)-2].name != anthropic.EvMessageDelta || fs[len(fs)-1].name != anthropic.EvMessageStop {
					t.Fatalf("missing terminal events: %v", names(fs))
				}
				last := fs[len(fs)-2].data
				wantStop := anthropic.StopEndTurn
				if terminal == EvIncomplete {
					wantStop = anthropic.StopMaxTokens
				}
				if got := last["delta"].(map[string]any)["stop_reason"]; got != wantStop {
					t.Errorf("stop_reason = %v, want %s", got, wantStop)
				}
				usage := last["usage"].(map[string]any)
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
}

func names(fs []frame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.name
	}
	return out
}

func textOf(fs []frame) string {
	var sb strings.Builder
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockDelta {
			continue
		}
		if d := f.data["delta"].(map[string]any); d["type"] == anthropic.DeltaText {
			sb.WriteString(d["text"].(string))
		}
	}
	return sb.String()
}

// argsByBlock reassembles streamed tool arguments, keyed by Anthropic block
// index. Interleaved fragments landing in the wrong block show up here.
func argsByBlock(fs []frame) map[float64]string {
	out := map[float64]string{}
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockDelta {
			continue
		}
		if d := f.data["delta"].(map[string]any); d["type"] == anthropic.DeltaInputJSON {
			out[f.data["index"].(float64)] += d["partial_json"].(string)
		}
	}
	return out
}

func idByBlock(fs []frame) map[float64]string {
	out := map[float64]string{}
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockStart {
			continue
		}
		b := f.data["content_block"].(map[string]any)
		if id, ok := b["id"].(string); ok {
			out[f.data["index"].(float64)] = id
		}
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func TestStreamPlainText(t *testing.T) {
	body := sse(t, map[string]any{"type": EvCreated, "response": map[string]any{"id": "resp-1"}}) +
		sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
			"item": map[string]any{"type": ItemMessage, "role": RoleAssistant}}) +
		sse(t, map[string]any{"type": EvOutputTextDelta, "output_index": 0, "delta": "Hel"}) +
		sse(t, map[string]any{"type": EvOutputTextDelta, "output_index": 0, "delta": "lo"}) +
		sse(t, map[string]any{"type": EvOutputItemDone, "output_index": 0}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{
			"id": "resp-1", "usage": map[string]any{"input_tokens": 12, "output_tokens": 3}}})

	fs := runStream(t, body, StreamOptions{})
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
		t.Fatalf("events = %v, want %v", got, want)
	}
	if got := textOf(fs); got != "Hello" {
		t.Errorf("text = %q", got)
	}
	md := fs[len(fs)-2]
	if md.data["delta"].(map[string]any)["stop_reason"] != anthropic.StopEndTurn {
		t.Errorf("stop_reason = %v", md.data["delta"])
	}
	if md.data["usage"].(map[string]any)["output_tokens"].(float64) != 3 {
		t.Errorf("usage = %v", md.data["usage"])
	}
}

func TestStreamFunctionCall(t *testing.T) {
	body := sse(t, map[string]any{"type": EvCreated, "response": map[string]any{"id": "r"}}) +
		sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
			"item": map[string]any{"type": ItemFunctionCall, "call_id": "call_1", "name": "Bash"}}) +
		sse(t, map[string]any{"type": EvFuncArgsDelta, "output_index": 0,
			"item_id": "fc_1", "delta": `{"comm`}) +
		sse(t, map[string]any{"type": EvFuncArgsDelta, "output_index": 0,
			"item_id": "fc_1", "delta": `and":"ls"}`}) +
		sse(t, map[string]any{"type": EvOutputItemDone, "output_index": 0}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"id": "r"}})

	fs := runStream(t, body, StreamOptions{})
	start := findFrame(t, fs, anthropic.EvContentBlockStart)
	block := start.data["content_block"].(map[string]any)
	if block["type"] != anthropic.BlockToolUse {
		t.Fatalf("block type = %v, want tool_use", block["type"])
	}
	// The call_id is what Claude Code will echo back as tool_use_id, so it has
	// to be the backend's own id.
	if block["id"] != "call_1" || block["name"] != "Bash" {
		t.Errorf("block = %v", block)
	}
	if got := argsByBlock(fs)[0]; got != `{"command":"ls"}` {
		t.Errorf("reassembled arguments = %q", got)
	}
	md := findFrame(t, fs, anthropic.EvMessageDelta)
	if md.data["delta"].(map[string]any)["stop_reason"] != anthropic.StopToolUse {
		t.Errorf("stop_reason = %v, want tool_use", md.data["delta"])
	}
}

func TestStreamInterleavedParallelToolCalls(t *testing.T) {
	// Two calls stream at once and their argument fragments interleave. Each
	// fragment must land in the block owning its output_index; getting this
	// wrong produces syntactically valid but wrong tool input, which fails
	// silently rather than loudly.
	body := sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
		"item": map[string]any{"type": ItemFunctionCall, "call_id": "call_a", "name": "Read"}}) +
		sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 1,
			"item": map[string]any{"type": ItemFunctionCall, "call_id": "call_b", "name": "Bash"}}) +
		sse(t, map[string]any{"type": EvFuncArgsDelta, "output_index": 0, "delta": `{"path":`}) +
		sse(t, map[string]any{"type": EvFuncArgsDelta, "output_index": 1, "delta": `{"command":`}) +
		sse(t, map[string]any{"type": EvFuncArgsDelta, "output_index": 0, "delta": `"a.txt"}`}) +
		sse(t, map[string]any{"type": EvFuncArgsDelta, "output_index": 1, "delta": `"ls"}`}) +
		sse(t, map[string]any{"type": EvOutputItemDone, "output_index": 0}) +
		sse(t, map[string]any{"type": EvOutputItemDone, "output_index": 1}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"id": "r"}})

	fs := runStream(t, body, StreamOptions{})
	ids := idByBlock(fs)
	args := argsByBlock(fs)

	var blockA, blockB float64 = -1, -1
	for idx, id := range ids {
		switch id {
		case "call_a":
			blockA = idx
		case "call_b":
			blockB = idx
		}
	}
	if blockA < 0 || blockB < 0 || blockA == blockB {
		t.Fatalf("expected two distinct blocks, got ids %v", ids)
	}
	if args[blockA] != `{"path":"a.txt"}` {
		t.Errorf("call_a arguments = %q", args[blockA])
	}
	if args[blockB] != `{"command":"ls"}` {
		t.Errorf("call_b arguments = %q", args[blockB])
	}
}

func TestStreamTextThenToolUsesSeparateBlocks(t *testing.T) {
	body := sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
		"item": map[string]any{"type": ItemMessage, "role": RoleAssistant}}) +
		sse(t, map[string]any{"type": EvOutputTextDelta, "output_index": 0, "delta": "thinking"}) +
		sse(t, map[string]any{"type": EvOutputItemDone, "output_index": 0}) +
		sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 1,
			"item": map[string]any{"type": ItemFunctionCall, "call_id": "c", "name": "Bash"}}) +
		sse(t, map[string]any{"type": EvFuncArgsDelta, "output_index": 1, "delta": "{}"}) +
		sse(t, map[string]any{"type": EvOutputItemDone, "output_index": 1}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"id": "r"}})

	fs := runStream(t, body, StreamOptions{})
	var starts, stops int
	for _, f := range fs {
		switch f.name {
		case anthropic.EvContentBlockStart:
			starts++
		case anthropic.EvContentBlockStop:
			stops++
		}
	}
	if starts != 2 || stops != 2 {
		t.Errorf("starts=%d stops=%d, want 2 and 2 — every block must be closed", starts, stops)
	}
}

func TestStreamReasoningBecomesThinking(t *testing.T) {
	body := sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
		"item": map[string]any{"type": ItemReasoning}}) +
		sse(t, map[string]any{"type": EvReasoningTextDelta, "output_index": 0, "delta": "pondering"}) +
		sse(t, map[string]any{"type": EvOutputItemDone, "output_index": 0}) +
		sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 1,
			"item": map[string]any{"type": ItemMessage, "role": RoleAssistant}}) +
		sse(t, map[string]any{"type": EvOutputTextDelta, "output_index": 1, "delta": "answer"}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"id": "r"}})

	fs := runStream(t, body, StreamOptions{ReasoningMode: ReasoningAsThinking})
	first := findFrame(t, fs, anthropic.EvContentBlockStart)
	if first.data["content_block"].(map[string]any)["type"] != anthropic.BlockThinking {
		t.Fatalf("first block = %v, want thinking", first.data["content_block"])
	}
	if got := textOf(fs); got != "answer" {
		t.Errorf("text = %q", got)
	}
}

func TestStreamReasoningDropped(t *testing.T) {
	body := sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
		"item": map[string]any{"type": ItemReasoning}}) +
		sse(t, map[string]any{"type": EvReasoningTextDelta, "output_index": 0, "delta": "pondering"}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"id": "r"}})

	fs := runStream(t, body, StreamOptions{ReasoningMode: ReasoningDrop})
	for _, f := range fs {
		if f.name != anthropic.EvContentBlockStart {
			continue
		}
		if f.data["content_block"].(map[string]any)["type"] == anthropic.BlockThinking {
			t.Error("a thinking block was emitted although reasoning is set to drop")
		}
	}
}

func TestStreamFailureBecomesAnErrorEvent(t *testing.T) {
	body := sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
		"item": map[string]any{"type": ItemMessage, "role": RoleAssistant}}) +
		sse(t, map[string]any{"type": EvOutputTextDelta, "output_index": 0, "delta": "partial"}) +
		sse(t, map[string]any{"type": EvFailed, "response": map[string]any{
			"error": map[string]any{"code": "rate_limit_exceeded", "message": "Rate limit reached"}}})

	fs := runStream(t, body, StreamOptions{})
	last := fs[len(fs)-1]
	if last.name != anthropic.EvError {
		t.Fatalf("last event = %q, want error", last.name)
	}
	e := last.data["error"].(map[string]any)
	if e["type"] != "rate_limit_exceeded" || e["message"] != "Rate limit reached" {
		t.Errorf("error = %v", e)
	}
}

func TestStreamIncompleteIsMaxTokensNotAFailure(t *testing.T) {
	body := sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
		"item": map[string]any{"type": ItemMessage, "role": RoleAssistant}}) +
		sse(t, map[string]any{"type": EvOutputTextDelta, "output_index": 0, "delta": "cut off"}) +
		sse(t, map[string]any{"type": EvIncomplete, "response": map[string]any{
			"incomplete_details": map[string]any{"reason": "max_output_tokens"}}})

	fs := runStream(t, body, StreamOptions{})
	md := findFrame(t, fs, anthropic.EvMessageDelta)
	if md.data["delta"].(map[string]any)["stop_reason"] != anthropic.StopMaxTokens {
		t.Errorf("stop_reason = %v, want max_tokens", md.data["delta"])
	}
	for _, f := range fs {
		if f.name == anthropic.EvError {
			t.Error("an incomplete response is not an error")
		}
	}
}

func TestStreamAlwaysTerminates(t *testing.T) {
	// Claude Code waits for message_stop; a stream that ends without one hangs
	// the UI.
	fs := runStream(t, "", StreamOptions{})
	if got := names(fs); !equal(got, []string{
		anthropic.EvMessageStart, anthropic.EvMessageDelta, anthropic.EvMessageStop,
	}) {
		t.Errorf("events = %v", got)
	}
}

func TestStreamClosesBlocksLeftOpenByATruncatedStream(t *testing.T) {
	// No output_item.done, no completed: the upstream just stopped.
	body := sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
		"item": map[string]any{"type": ItemFunctionCall, "call_id": "c", "name": "Bash"}}) +
		sse(t, map[string]any{"type": EvFuncArgsDelta, "output_index": 0, "delta": "{}"})

	fs := runStream(t, body, StreamOptions{})
	var starts, stops int
	for _, f := range fs {
		switch f.name {
		case anthropic.EvContentBlockStart:
			starts++
		case anthropic.EvContentBlockStop:
			stops++
		}
	}
	if starts != stops {
		t.Errorf("starts=%d stops=%d — an unterminated block leaves the parser hanging", starts, stops)
	}
	if names(fs)[len(fs)-1] != anthropic.EvMessageStop {
		t.Errorf("stream did not end with message_stop: %v", names(fs))
	}
}

func TestStreamIgnoresUnknownAndUnparseableEvents(t *testing.T) {
	body := "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n" +
		"data: {not json\n\n" +
		sse(t, map[string]any{"type": EvOutputItemAdded, "output_index": 0,
			"item": map[string]any{"type": ItemMessage, "role": RoleAssistant}}) +
		sse(t, map[string]any{"type": EvOutputTextDelta, "output_index": 0, "delta": "ok"}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"id": "r"}})

	fs := runStream(t, body, StreamOptions{})
	if got := textOf(fs); got != "ok" {
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

// messageStartID pulls the id out of a translated stream's message_start.
func messageStartID(t *testing.T, fs []frame) string {
	t.Helper()
	if len(fs) == 0 || fs[0].name != anthropic.EvMessageStart {
		t.Fatalf("expected message_start first, got %v", names(fs))
	}
	id, ok := fs[0].data["message"].(map[string]any)["id"].(string)
	if !ok {
		t.Fatalf("message_start carries no string id: %v", fs[0].data)
	}
	return id
}

func TestStreamMessageIDComesFromTheResponse(t *testing.T) {
	body := sse(t, map[string]any{"type": EvCreated, "response": map[string]any{"id": "resp_abc123"}}) +
		sse(t, map[string]any{"type": EvOutputTextDelta, "delta": "Hi"}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"usage": json.RawMessage(`{"input_tokens":10,"output_tokens":1}`)}})
	if got, want := messageStartID(t, runStream(t, body, StreamOptions{})), "msg_resp_abc123"; got != want {
		t.Errorf("message id = %q, want %q", got, want)
	}
}

// Claude Code groups a response's messages by this id, so two responses must
// never share one: it walks back through same-id messages to find where the
// current response began, and dedupes its cumulative token count on it. See
// anthropic.NewMessageID.
func TestStreamMessageIDsDifferWhenTheResponseHasNoID(t *testing.T) {
	body := sse(t, map[string]any{"type": EvCreated, "response": map[string]any{}}) +
		sse(t, map[string]any{"type": EvOutputTextDelta, "delta": "Hi"}) +
		sse(t, map[string]any{"type": EvCompleted, "response": map[string]any{"usage": json.RawMessage(`{"input_tokens":10,"output_tokens":1}`)}})
	first := messageStartID(t, runStream(t, body, StreamOptions{}))
	second := messageStartID(t, runStream(t, body, StreamOptions{}))
	if first == second {
		t.Errorf("both responses claim id %q; every response needs its own", first)
	}
	for _, id := range []string{first, second} {
		if !strings.HasPrefix(id, "msg_") {
			t.Errorf("id %q does not look like an Anthropic message id", id)
		}
	}
}

// A zero total is indistinguishable from a real measurement to Claude Code: it
// divides it by the window and reports no context in use. The estimate that
// seeded message_start is the better answer when the backend reports nothing.
func TestStreamFinalUsageFallsBackToTheEstimate(t *testing.T) {
	for _, terminal := range []string{EvCompleted, EvIncomplete} {
		t.Run(terminal, func(t *testing.T) {
			body := sse(t, map[string]any{"type": EvOutputTextDelta, "delta": "Hello"}) +
				sse(t, map[string]any{"type": terminal, "response": map[string]any{"usage": nil}})
			fs := runStreamWithEstimate(t, body, StreamOptions{}, 4242)
			if len(fs) < 2 || fs[len(fs)-2].name != anthropic.EvMessageDelta {
				t.Fatalf("expected message_delta before message_stop, got %v", names(fs))
			}
			usage := fs[len(fs)-2].data["usage"].(map[string]any)
			if got := usage["input_tokens"]; got != float64(4242) {
				t.Errorf("message_delta input_tokens = %v, want 4242 (the estimate, not zero)", got)
			}
		})
	}
}
