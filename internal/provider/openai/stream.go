package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bfreis/claude-code-llm-gateway/internal/anthropic"
)

// blockKind is the type of the Anthropic content block currently open.
type blockKind int

const (
	blockNone blockKind = iota
	blockThinking
	blockText
	blockTool
)

// StreamTranslator turns an OpenAI Chat Completions SSE stream into the
// Anthropic Messages SSE stream Claude Code expects.
//
// Anthropic's protocol is block-structured — every piece of content is framed
// by content_block_start / content_block_stop with a sequential index — while
// OpenAI's is a flat sequence of deltas. This type holds the block state needed
// to bridge the two.
type StreamTranslator struct {
	out     *anthropic.StreamWriter
	modelID string
	opt     Options

	started   bool
	curKind   blockKind
	curIndex  int
	nextIndex int

	// toolBlock maps an OpenAI tool_calls index to the Anthropic block index
	// already opened for it.
	toolBlock map[int]int

	usage        anthropic.Usage
	stopReason   string
	sawToolCalls bool
}

// NewStreamTranslator prepares a translator writing to out.
func NewStreamTranslator(out *anthropic.StreamWriter, modelID string, opt Options) *StreamTranslator {
	return &StreamTranslator{
		out:       out,
		modelID:   modelID,
		opt:       opt,
		toolBlock: map[int]int{},
	}
}

// Run consumes the whole upstream stream and emits the translated one.
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
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		// An error object can arrive in place of a chunk.
		if kind, msg, ok := decodeStreamError(data); ok {
			return t.fail(kind, msg)
		}
		var chunk StreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// A frame we cannot parse is not worth killing the turn over.
			continue
		}
		if err := t.chunk(&chunk); err != nil {
			return err
		}
	}
	return t.finish()
}

func (t *StreamTranslator) chunk(c *StreamChunk) error {
	if err := t.ensureStarted(); err != nil {
		return err
	}
	if c.Usage != nil {
		t.usage = translateUsage(c.Usage)
	}
	for i := range c.Choices {
		if err := t.choice(&c.Choices[i]); err != nil {
			return err
		}
	}
	return nil
}

func (t *StreamTranslator) choice(ch *StreamChoice) error {
	d := ch.Delta

	if r := d.ReasoningText(); r != "" && t.opt.ReasoningMode == ReasoningAsThinking {
		if err := t.openBlock(blockThinking, anthropic.ContentBlock{
			Type: anthropic.BlockThinking, Thinking: "", Signature: "",
		}); err != nil {
			return err
		}
		if err := t.out.Event(anthropic.EvContentBlockDelta, anthropic.ContentBlockDeltaEvent{
			Type:  anthropic.EvContentBlockDelta,
			Index: t.curIndex,
			Delta: anthropic.Delta{Type: anthropic.DeltaThinking, Thinking: r},
		}); err != nil {
			return err
		}
	}

	if d.Content != nil && *d.Content != "" {
		if err := t.openBlock(blockText, anthropic.ContentBlock{Type: anthropic.BlockText, Text: ""}); err != nil {
			return err
		}
		if err := t.out.Event(anthropic.EvContentBlockDelta, anthropic.ContentBlockDeltaEvent{
			Type:  anthropic.EvContentBlockDelta,
			Index: t.curIndex,
			Delta: anthropic.Delta{Type: anthropic.DeltaText, Text: *d.Content},
		}); err != nil {
			return err
		}
	}

	for _, tc := range d.ToolCalls {
		if err := t.toolCall(tc); err != nil {
			return err
		}
	}

	if ch.FinishReason != nil && *ch.FinishReason != "" {
		t.stopReason = TranslateStopReason(*ch.FinishReason, t.sawToolCalls)
	}
	return nil
}

// toolCall handles one streamed tool_calls fragment. The first fragment for an
// OpenAI index carries the id and function name and opens a block; later
// fragments carry only argument text.
func (t *StreamTranslator) toolCall(tc ToolCall) error {
	t.sawToolCalls = true

	idx, open := t.toolBlock[tc.Index]
	if !open {
		if err := t.openBlock(blockTool, anthropic.ContentBlock{
			Type:  anthropic.BlockToolUse,
			ID:    toolUseID(tc.ID, tc.Index),
			Name:  tc.Function.Name,
			Input: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		t.toolBlock[tc.Index] = t.curIndex
		idx = t.curIndex
	}

	if tc.Function.Arguments == "" {
		return nil
	}
	// Arguments for an earlier tool call can interleave; address the block by
	// its recorded index rather than assuming it is still the open one.
	return t.out.Event(anthropic.EvContentBlockDelta, anthropic.ContentBlockDeltaEvent{
		Type:  anthropic.EvContentBlockDelta,
		Index: idx,
		Delta: anthropic.Delta{Type: anthropic.DeltaInputJSON, PartialJSON: tc.Function.Arguments},
	})
}

func toolUseID(id string, index int) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("toolu_gateway_%d", index)
}

// openBlock closes any open block and starts a new one, unless a block of the
// same kind is already open and can absorb the delta.
func (t *StreamTranslator) openBlock(kind blockKind, block anthropic.ContentBlock) error {
	if t.curKind == kind && kind != blockTool {
		return nil
	}
	if err := t.closeBlock(); err != nil {
		return err
	}
	t.curIndex = t.nextIndex
	t.nextIndex++
	t.curKind = kind
	return t.out.Event(anthropic.EvContentBlockStart, anthropic.ContentBlockStartEvent{
		Type:         anthropic.EvContentBlockStart,
		Index:        t.curIndex,
		ContentBlock: block,
	})
}

func (t *StreamTranslator) closeBlock() error {
	if t.curKind == blockNone {
		return nil
	}
	t.curKind = blockNone
	return t.out.Event(anthropic.EvContentBlockStop, anthropic.ContentBlockStopEvent{
		Type:  anthropic.EvContentBlockStop,
		Index: t.curIndex,
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
			ID:      "msg_gateway",
			Type:    "message",
			Role:    anthropic.RoleAssistant,
			Model:   t.modelID,
			Content: []anthropic.ContentBlock{},
			Usage:   anthropic.Usage{},
		},
	})
}

// finish closes the stream with the terminal events Claude Code waits for.
func (t *StreamTranslator) finish() error {
	if err := t.ensureStarted(); err != nil {
		return err
	}
	if err := t.closeBlock(); err != nil {
		return err
	}
	stop := t.stopReason
	if stop == "" {
		stop = anthropic.StopEndTurn
	}
	if err := t.out.Event(anthropic.EvMessageDelta, anthropic.MessageDeltaEvent{
		Type:  anthropic.EvMessageDelta,
		Delta: anthropic.MessageDeltaBody{StopReason: &stop},
		Usage: anthropic.MessageDeltaUsage{
			InputTokens:              t.usage.InputTokens,
			OutputTokens:             t.usage.OutputTokens,
			CacheCreationInputTokens: t.usage.CacheCreationInputTokens,
			CacheReadInputTokens:     t.usage.CacheReadInputTokens,
		},
	}); err != nil {
		return err
	}
	return t.out.Event(anthropic.EvMessageStop, anthropic.MessageStopEvent{Type: anthropic.EvMessageStop})
}

// fail reports an upstream failure in-band. Once the SSE response has begun,
// the status line is already sent, so an error event is the only way to tell
// Claude Code what went wrong.
func (t *StreamTranslator) fail(kind, msg string) error {
	if err := t.ensureStarted(); err != nil {
		return err
	}
	_ = t.closeBlock()
	return t.out.Event(anthropic.EvError, anthropic.NewErrorEvent(kind, msg))
}

// decodeStreamError recognises an error object sent in place of a chunk.
func decodeStreamError(data string) (kind, msg string, ok bool) {
	var probe ErrorResponse
	if err := json.Unmarshal([]byte(data), &probe); err != nil {
		return "", "", false
	}
	if probe.Error.Message == "" {
		return "", "", false
	}
	kind = probe.Error.Type
	if kind == "" {
		kind = "api_error"
	}
	return kind, probe.Error.Message, true
}
