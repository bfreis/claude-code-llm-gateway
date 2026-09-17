package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SSE event names of the Anthropic streaming protocol.
const (
	EvMessageStart      = "message_start"
	EvContentBlockStart = "content_block_start"
	EvContentBlockDelta = "content_block_delta"
	EvContentBlockStop  = "content_block_stop"
	EvMessageDelta      = "message_delta"
	EvMessageStop       = "message_stop"
	EvPing              = "ping"
	EvError             = "error"
)

// StreamWriter emits Anthropic SSE events to an http.ResponseWriter.
//
// Every event is flushed immediately: Claude Code arms a first-byte watchdog on
// streaming responses and reports "a proxy or gateway that buffers streaming
// responses can cause this" if the first bytes are held back.
type StreamWriter struct {
	mu       sync.Mutex
	w        io.Writer
	flusher  http.Flusher
	err      error
	lastSent time.Time
}

// KeepaliveInterval is how often an otherwise-silent stream emits a ping.
//
// Claude Code aborts a stream after 300s with no events (and warns the user at
// 150s). A reasoning model that thinks for minutes before its first token would
// trip that, so the gateway keeps the stream alive. Ping events are accepted
// anywhere in the stream, including before message_start.
const KeepaliveInterval = 60 * time.Second

// NewStreamWriter prepares w for SSE and writes the response headers.
func NewStreamWriter(w http.ResponseWriter) *StreamWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sw := &StreamWriter{w: w, lastSent: time.Now()}
	if f, ok := w.(http.Flusher); ok {
		sw.flusher = f
		f.Flush()
	}
	return sw
}

// Event marshals v and writes it as a named SSE event.
//
// Safe for concurrent use: the keepalive goroutine writes through the same
// StreamWriter as the translator.
func (s *StreamWriter) Event(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.event(name, v)
}

func (s *StreamWriter) event(name string, v any) error {
	if s.err != nil {
		return s.err
	}
	s.lastSent = time.Now()
	payload, err := json.Marshal(v)
	if err != nil {
		s.err = fmt.Errorf("marshal %s event: %w", name, err)
		return s.err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, payload); err != nil {
		s.err = err
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

// Err returns the first write error, if any.
func (s *StreamWriter) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Keepalive emits a ping whenever the stream has been silent for interval,
// returning when ctx is cancelled. Run it in a goroutine for the lifetime of a
// streamed response.
func (s *StreamWriter) Keepalive(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = KeepaliveInterval
	}
	t := time.NewTicker(interval / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			idle := time.Since(s.lastSent) >= interval
			if idle && s.err == nil {
				_ = s.event(EvPing, map[string]string{"type": EvPing})
			}
			s.mu.Unlock()
		}
	}
}

// Envelope types for the events the gateway emits.

type MessageStartEvent struct {
	Type    string           `json:"type"`
	Message MessageStartBody `json:"message"`
}

type MessageStartBody struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

type ContentBlockStartEvent struct {
	Type         string       `json:"type"`
	Index        int          `json:"index"`
	ContentBlock ContentBlock `json:"content_block"`
}

type ContentBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta Delta  `json:"delta"`
}

// Delta carries the per-chunk payload. Only one field is set at a time.
type Delta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

// Delta type discriminators.
const (
	DeltaText      = "text_delta"
	DeltaInputJSON = "input_json_delta"
	DeltaThinking  = "thinking_delta"
	DeltaSignature = "signature_delta"
)

type ContentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type MessageDeltaEvent struct {
	Type  string            `json:"type"`
	Delta MessageDeltaBody  `json:"delta"`
	Usage MessageDeltaUsage `json:"usage"`
}

type MessageDeltaBody struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type MessageDeltaUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type MessageStopEvent struct {
	Type string `json:"type"`
}

type ErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// NewErrorEvent builds an in-band stream error.
func NewErrorEvent(kind, msg string) ErrorEvent {
	var e ErrorEvent
	e.Type = EvError
	e.Error.Type = kind
	e.Error.Message = msg
	return e
}

// Reader parses an SSE stream into (event, data) pairs.
//
// The Anthropic and OpenAI streams are both read with this: OpenAI omits the
// event: line and sends only data:, so Name is empty for those frames.
type Reader struct {
	sc *bufio.Scanner
}

// Frame is one parsed SSE message.
type Frame struct {
	Name string
	Data string
}

// NewReader wraps an SSE body.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	// Tool-heavy frames are large; give the scanner room before it errors.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &Reader{sc: sc}
}

// Next returns the next frame, or io.EOF when the stream ends.
func (r *Reader) Next() (Frame, error) {
	var f Frame
	var data []string
	for r.sc.Scan() {
		line := r.sc.Text()
		switch {
		case line == "":
			if len(data) == 0 && f.Name == "" {
				continue // stray blank line between frames
			}
			f.Data = strings.Join(data, "\n")
			return f, nil
		case strings.HasPrefix(line, ":"):
			continue // comment / keepalive
		case strings.HasPrefix(line, "event:"):
			f.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := r.sc.Err(); err != nil {
		return f, err
	}
	if len(data) > 0 || f.Name != "" {
		f.Data = strings.Join(data, "\n")
		return f, nil
	}
	return f, io.EOF
}
