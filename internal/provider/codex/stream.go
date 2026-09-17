package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

// Responses SSE event names (codex-rs/codex-api/src/sse/responses.rs and the
// fixtures in codex-rs/core/tests/common/responses.rs).
const (
	EvCreated            = "response.created"
	EvOutputItemAdded    = "response.output_item.added"
	EvOutputItemDone     = "response.output_item.done"
	EvOutputTextDelta    = "response.output_text.delta"
	EvFuncArgsDelta      = "response.function_call_arguments.delta"
	EvFuncArgsDone       = "response.function_call_arguments.done"
	EvReasoningTextDelta = "response.reasoning_text.delta"
	EvReasoningSumDelta  = "response.reasoning_summary_text.delta"
	EvCompleted          = "response.completed"
	EvFailed             = "response.failed"
	EvIncomplete         = "response.incomplete"
)

// StreamEvent is the flat envelope every Responses SSE frame decodes into.
//
// The API does not use a tagged union: one shape carries every event and the
// `type` field selects which fields are meaningful.
type StreamEvent struct {
	Type string `json:"type"`

	Response    *ResponseBody `json:"response,omitempty"`
	Item        *Item         `json:"item,omitempty"`
	ItemID      string        `json:"item_id,omitempty"`
	CallID      string        `json:"call_id,omitempty"`
	OutputIndex *int          `json:"output_index,omitempty"`
	Delta       string        `json:"delta,omitempty"`
	Text        string        `json:"text,omitempty"`
	Arguments   string        `json:"arguments,omitempty"`
	ContentIdx  *int          `json:"content_index,omitempty"`
	SummaryIdx  *int          `json:"summary_index,omitempty"`
}

// ResponseBody is the `response` object on lifecycle events.
type ResponseBody struct {
	ID                string          `json:"id,omitempty"`
	Usage             *Usage          `json:"usage,omitempty"`
	Error             *ResponseError  `json:"error,omitempty"`
	IncompleteDetails *IncompleteBody `json:"incomplete_details,omitempty"`
}

// Usage is the token accounting on response.completed.
type Usage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
}

// ResponseError is the failure payload on response.failed.
type ResponseError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// IncompleteBody explains a truncated response.
type IncompleteBody struct {
	Reason string `json:"reason,omitempty"`
}

// blockKind is the type of the Anthropic content block currently open.
type blockKind int

const (
	blockNone blockKind = iota
	blockThinking
	blockText
	blockTool
)

// StreamTranslator converts a Codex Responses SSE stream into the Anthropic
// Messages SSE stream Claude Code expects.
//
// The two protocols disagree about structure. Responses emits a flat sequence
// of items identified by `output_index`, which may interleave; Anthropic frames
// content in blocks with sequential indices that must be opened and closed in
// order. This type keeps the mapping from one to the other.
type StreamTranslator struct {
	out     *anthropic.StreamWriter
	modelID string
	opt     StreamOptions

	started   bool
	curKind   blockKind
	curIndex  int
	nextIndex int

	// blockFor maps a Responses output_index to the Anthropic block index
	// opened for it, so a delta that arrives after another item has opened
	// still lands in the right block.
	blockFor map[int]int

	usage       Usage
	stopReason  string
	sawToolCall bool
	failed      bool

	// estimatedInputTokens seeds message_start's usage so Claude Code's
	// context display does not flash to zero before the real count arrives
	// in message_delta. See anthropic.EstimateInputTokens.
	estimatedInputTokens int
}

// StreamOptions tune the translated stream.
type StreamOptions struct {
	// ReasoningMode decides what happens to reasoning text.
	ReasoningMode ReasoningMode
}

// ReasoningMode selects the handling of reasoning text.
type ReasoningMode string

const (
	// ReasoningAsThinking surfaces reasoning as Anthropic thinking blocks.
	ReasoningAsThinking ReasoningMode = "thinking"
	// ReasoningDrop discards reasoning text.
	ReasoningDrop ReasoningMode = "drop"
)

// NewStreamTranslator prepares a translator writing to out.
func NewStreamTranslator(out *anthropic.StreamWriter, modelID string, opt StreamOptions, estimatedInputTokens int) *StreamTranslator {
	return &StreamTranslator{
		out:                  out,
		modelID:              modelID,
		opt:                  opt,
		blockFor:             map[int]int{},
		estimatedInputTokens: estimatedInputTokens,
	}
}

// Run consumes the upstream stream and emits the translated one.
func (t *StreamTranslator) Run(body io.Reader) error {
	r := anthropic.NewReader(body)
	for {
		frame, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return t.fail("api_error", fmt.Sprintf("reading upstream stream: %v", err))
		}
		data := strings.TrimSpace(frame.Data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev StreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // a frame we cannot parse is not worth killing the turn over
		}
		if err := t.event(&ev); err != nil {
			return err
		}
		if t.failed {
			return nil
		}
	}
	return t.finish()
}

func (t *StreamTranslator) event(ev *StreamEvent) error {
	switch ev.Type {
	case EvCreated:
		return t.ensureStarted()

	case EvOutputItemAdded:
		return t.itemAdded(ev)

	case EvOutputTextDelta:
		if ev.Delta == "" {
			return nil
		}
		if err := t.openBlock(ev.OutputIndex, blockText,
			anthropic.ContentBlock{Type: anthropic.BlockText}); err != nil {
			return err
		}
		return t.delta(ev.OutputIndex, anthropic.Delta{
			Type: anthropic.DeltaText, Text: ev.Delta,
		})

	case EvReasoningTextDelta, EvReasoningSumDelta:
		if ev.Delta == "" || t.opt.ReasoningMode == ReasoningDrop {
			return nil
		}
		if err := t.openBlock(ev.OutputIndex, blockThinking,
			anthropic.ContentBlock{Type: anthropic.BlockThinking}); err != nil {
			return err
		}
		return t.delta(ev.OutputIndex, anthropic.Delta{
			Type: anthropic.DeltaThinking, Thinking: ev.Delta,
		})

	case EvFuncArgsDelta:
		if ev.Delta == "" {
			return nil
		}
		// The block was opened by output_item.added; if that was missed, open
		// one now so the arguments are not silently dropped.
		if err := t.openBlock(ev.OutputIndex, blockTool, anthropic.ContentBlock{
			Type:  anthropic.BlockToolUse,
			ID:    toolUseID(ev.CallID, ev.ItemID, ev.OutputIndex),
			Input: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		return t.delta(ev.OutputIndex, anthropic.Delta{
			Type: anthropic.DeltaInputJSON, PartialJSON: ev.Delta,
		})

	case EvOutputItemDone:
		// A completed reasoning item carries the encrypted blob that preserves
		// the model's chain of thought across turns. Anthropic has no field for
		// it, so it rides out in the thinking block's signature and is decoded
		// again when the conversation is replayed.
		if ev.Item != nil && ev.Item.Type == ItemReasoning &&
			ev.Item.EncryptedContent != "" && t.opt.ReasoningMode != ReasoningDrop {
			if sig := EncodeReasoning(ev.Item.ID, ev.Item.EncryptedContent); sig != "" {
				if err := t.delta(ev.OutputIndex, anthropic.Delta{
					Type: anthropic.DeltaSignature, Signature: sig,
				}); err != nil {
					return err
				}
			}
		}
		return t.closeIndex(ev.OutputIndex)

	case EvCompleted:
		if ev.Response != nil && ev.Response.Usage != nil {
			t.usage = *ev.Response.Usage
		}
		return nil

	case EvFailed:
		kind, msg := "api_error", "the upstream response failed"
		if ev.Response != nil && ev.Response.Error != nil {
			if ev.Response.Error.Code != "" {
				kind = ev.Response.Error.Code
			}
			if ev.Response.Error.Message != "" {
				msg = ev.Response.Error.Message
			}
		}
		return t.fail(kind, msg)

	case EvIncomplete:
		// Not a failure: the turn produced content and stopped early.
		t.stopReason = anthropic.StopMaxTokens
		if ev.Response != nil && ev.Response.Usage != nil {
			t.usage = *ev.Response.Usage
		}
		return nil
	}
	return nil
}

// itemAdded opens a block for a newly announced item. The item carries its own
// type, and for a function call its call_id and name.
func (t *StreamTranslator) itemAdded(ev *StreamEvent) error {
	if ev.Item == nil {
		return nil
	}
	switch ev.Item.Type {
	case ItemFunctionCall:
		t.sawToolCall = true
		return t.openBlock(ev.OutputIndex, blockTool, anthropic.ContentBlock{
			Type:  anthropic.BlockToolUse,
			ID:    toolUseID(ev.Item.CallID, ev.ItemID, ev.OutputIndex),
			Name:  ev.Item.Name,
			Input: json.RawMessage(`{}`),
		})
	case ItemMessage:
		return t.openBlock(ev.OutputIndex, blockText, anthropic.ContentBlock{Type: anthropic.BlockText})
	case ItemReasoning:
		if t.opt.ReasoningMode == ReasoningDrop {
			return nil
		}
		return t.openBlock(ev.OutputIndex, blockThinking,
			anthropic.ContentBlock{Type: anthropic.BlockThinking})
	}
	return nil
}

// toolUseID picks a stable id for a tool call, preferring the backend's own.
func toolUseID(callID, itemID string, outputIndex *int) string {
	if callID != "" {
		return callID
	}
	if itemID != "" {
		return itemID
	}
	idx := 0
	if outputIndex != nil {
		idx = *outputIndex
	}
	return fmt.Sprintf("toolu_gateway_%d", idx)
}

// key turns an optional output_index into a map key. Events without one are
// folded onto a single implicit stream, which is correct for a response that
// never interleaves.
func key(outputIndex *int) int {
	if outputIndex == nil {
		return 0
	}
	return *outputIndex
}

// openBlock ensures a block exists for this output index, opening one if not.
//
// Several blocks may be open at once. Codex interleaves items freely — two
// parallel tool calls stream their arguments in alternating fragments — and
// closing the previous block on each new one would strand every later fragment
// belonging to the earlier call. Anthropic's own API never interleaves, but its
// stream parser appends blocks in arrival order and addresses deltas by index,
// so concurrent blocks reassemble correctly as long as the starts are emitted
// in increasing index order, which nextIndex guarantees.
func (t *StreamTranslator) openBlock(outputIndex *int, kind blockKind, block anthropic.ContentBlock) error {
	if err := t.ensureStarted(); err != nil {
		return err
	}
	k := key(outputIndex)
	if _, open := t.blockFor[k]; open {
		return nil
	}

	idx := t.nextIndex
	t.nextIndex++
	t.blockFor[k] = idx
	t.curIndex = idx
	t.curKind = kind

	return t.out.Event(anthropic.EvContentBlockStart, anthropic.ContentBlockStartEvent{
		Type:         anthropic.EvContentBlockStart,
		Index:        idx,
		ContentBlock: block,
	})
}

// delta emits a content delta into the block owning this output index.
func (t *StreamTranslator) delta(outputIndex *int, d anthropic.Delta) error {
	idx, ok := t.blockFor[key(outputIndex)]
	if !ok {
		idx = t.curIndex
	}
	return t.out.Event(anthropic.EvContentBlockDelta, anthropic.ContentBlockDeltaEvent{
		Type:  anthropic.EvContentBlockDelta,
		Index: idx,
		Delta: d,
	})
}

// closeIndex closes the block owning an output index.
func (t *StreamTranslator) closeIndex(outputIndex *int) error {
	k := key(outputIndex)
	idx, ok := t.blockFor[k]
	if !ok {
		return nil
	}
	delete(t.blockFor, k)
	if idx == t.curIndex {
		t.curKind = blockNone
	}
	return t.out.Event(anthropic.EvContentBlockStop, anthropic.ContentBlockStopEvent{
		Type:  anthropic.EvContentBlockStop,
		Index: idx,
	})
}

func (t *StreamTranslator) ensureStarted() error {
	if t.started {
		return nil
	}
	t.started = true
	return t.out.Event(anthropic.EvMessageStart, anthropic.MessageStartEvent{
		Type: anthropic.EvMessageStart,
		Message: anthropic.MessageStartBody{
			ID:      "msg_codex",
			Type:    "message",
			Role:    anthropic.RoleAssistant,
			Model:   t.modelID,
			Content: []anthropic.ContentBlock{},
			Usage:   anthropic.Usage{InputTokens: t.estimatedInputTokens},
		},
	})
}

// finish closes every open block and emits the terminal events.
func (t *StreamTranslator) finish() error {
	if err := t.ensureStarted(); err != nil {
		return err
	}
	if err := t.closeAll(); err != nil {
		return err
	}

	stop := t.stopReason
	if stop == "" {
		if t.sawToolCall {
			stop = anthropic.StopToolUse
		} else {
			stop = anthropic.StopEndTurn
		}
	}
	usage := anthropic.MessageDeltaUsage{
		InputTokens:  t.usage.InputTokens,
		OutputTokens: t.usage.OutputTokens,
	}
	if details := t.usage.InputTokensDetails; details != nil && details.CachedTokens > 0 {
		usage.CacheReadInputTokens = details.CachedTokens
		usage.InputTokens = max(0, t.usage.InputTokens-details.CachedTokens)
	}
	if err := t.out.Event(anthropic.EvMessageDelta, anthropic.MessageDeltaEvent{
		Type:  anthropic.EvMessageDelta,
		Delta: anthropic.MessageDeltaBody{StopReason: &stop},
		Usage: usage,
	}); err != nil {
		return err
	}
	return t.out.Event(anthropic.EvMessageStop, anthropic.MessageStopEvent{Type: anthropic.EvMessageStop})
}

// closeAll closes every block still open, lowest index first so the client sees
// them in a sensible order.
func (t *StreamTranslator) closeAll() error {
	for len(t.blockFor) > 0 {
		lowestKey, lowestIdx := 0, -1
		for k, idx := range t.blockFor {
			if lowestIdx == -1 || idx < lowestIdx {
				lowestKey, lowestIdx = k, idx
			}
		}
		delete(t.blockFor, lowestKey)
		if err := t.out.Event(anthropic.EvContentBlockStop, anthropic.ContentBlockStopEvent{
			Type:  anthropic.EvContentBlockStop,
			Index: lowestIdx,
		}); err != nil {
			return err
		}
	}
	t.curKind = blockNone
	return nil
}

// fail reports an upstream failure in-band. Once the SSE response has begun the
// status line is already sent, so an error event is the only way to say so.
func (t *StreamTranslator) fail(kind, msg string) error {
	t.failed = true
	if err := t.ensureStarted(); err != nil {
		return err
	}
	_ = t.closeAll()
	return t.out.Event(anthropic.EvError, anthropic.NewErrorEvent(kind, msg))
}
