package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStreamWriterEmitsNamedEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := NewStreamWriter(rec)
	if err := sw.Event(EvMessageStop, MessageStopEvent{Type: EvMessageStop}); err != nil {
		t.Fatalf("Event: %v", err)
	}
	got := rec.Body.String()
	// Claude Code's parser reads the event: line; data: alone is not enough
	// for the events it dispatches on.
	if !strings.HasPrefix(got, "event: message_stop\ndata: {") {
		t.Errorf("frame = %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("frame = %q, want a blank-line terminator", got)
	}
}

func TestStreamWriterSetsNoBufferHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	NewStreamWriter(rec)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Error("X-Accel-Buffering should disable proxy buffering")
	}
}

// syncRecorder is a ResponseWriter whose buffer is safe to read while the
// keepalive goroutine writes. httptest.ResponseRecorder is not.
type syncRecorder struct {
	mu  sync.Mutex
	buf strings.Builder
	hdr http.Header
}

func newSyncRecorder() *syncRecorder { return &syncRecorder{hdr: http.Header{}} }

func (r *syncRecorder) Header() http.Header { return r.hdr }
func (r *syncRecorder) WriteHeader(int)     {}
func (r *syncRecorder) Flush()              {}

func (r *syncRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(b)
}

func (r *syncRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func TestKeepalivePingsWhenIdle(t *testing.T) {
	rec := newSyncRecorder()
	sw := NewStreamWriter(rec)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sw.Keepalive(ctx, 40*time.Millisecond) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if !strings.Contains(rec.String(), "event: ping") {
		t.Errorf("no ping emitted during idle period:\n%s", rec.String())
	}
}

func TestKeepaliveStaysQuietWhileEventsFlow(t *testing.T) {
	rec := newSyncRecorder()
	sw := NewStreamWriter(rec)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sw.Keepalive(ctx, 200*time.Millisecond) }()
	for i := 0; i < 8; i++ {
		if err := sw.Event(EvContentBlockDelta, ContentBlockDeltaEvent{
			Type: EvContentBlockDelta, Delta: Delta{Type: DeltaText, Text: "x"},
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done

	if strings.Contains(rec.String(), "event: ping") {
		t.Error("ping emitted although the stream was never idle")
	}
}

func TestReaderParsesFramesAndIgnoresComments(t *testing.T) {
	body := ": keepalive comment\n" +
		"event: message_start\ndata: {\"a\":1}\n\n" +
		"data: {\"b\":2}\n\n" + // OpenAI style: no event: line
		"data: [DONE]\n\n"
	r := NewReader(strings.NewReader(body))

	want := []Frame{
		{Name: "message_start", Data: `{"a":1}`},
		{Name: "", Data: `{"b":2}`},
		{Name: "", Data: "[DONE]"},
	}
	for i, w := range want {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got != w {
			t.Errorf("frame %d = %+v, want %+v", i, got, w)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("err = %v, want EOF", err)
	}
}

func TestReaderJoinsMultilineData(t *testing.T) {
	r := NewReader(strings.NewReader("data: line1\ndata: line2\n\n"))
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.Data != "line1\nline2" {
		t.Errorf("Data = %q", got.Data)
	}
}

func TestDecodeContentAcceptsStringOrBlocks(t *testing.T) {
	blocks, err := DecodeContent([]byte(`"plain"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Type != BlockText || blocks[0].Text != "plain" {
		t.Errorf("blocks = %+v", blocks)
	}

	blocks, err = DecodeContent([]byte(`[{"type":"text","text":"a"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Text != "a" {
		t.Errorf("blocks = %+v", blocks)
	}
}
