// Package anthropic holds the subset of the Anthropic Messages API wire format
// that the gateway needs to understand in order to translate to and from other
// providers. Requests bound for Anthropic itself are proxied byte-for-byte and
// never pass through these types.
package anthropic

import "encoding/json"

// Version is the anthropic-version header value Claude Code sends.
const Version = "2023-06-01"

// MessagesRequest is POST /v1/messages.
//
// Fields the gateway does not interpret are preserved in Extra so that a
// translator can decide what to do with them and a passthrough never drops
// anything.
type MessagesRequest struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Thinking      *Thinking       `json:"thinking,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

// Thinking is the extended-thinking control block.
type Thinking struct {
	Type         string `json:"type"` // "enabled" | "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// Enabled reports whether extended thinking was requested.
func (t *Thinking) Enabled() bool { return t != nil && t.Type == "enabled" }

// Conversation roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is one conversation turn.
type Message struct {
	Role    string          `json:"role"` // "user" | "assistant"
	Content json.RawMessage `json:"content"`
}

// Blocks decodes Content, which may be a bare string or an array of blocks.
func (m Message) Blocks() ([]ContentBlock, error) {
	return DecodeContent(m.Content)
}

// DecodeContent normalises the string-or-array content shape into blocks.
func DecodeContent(raw json.RawMessage) ([]ContentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ContentBlock{{Type: BlockText, Text: s}}, nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// Block type discriminators.
const (
	BlockText             = "text"
	BlockImage            = "image"
	BlockDocument         = "document"
	BlockToolUse          = "tool_use"
	BlockToolResult       = "tool_result"
	BlockThinking         = "thinking"
	BlockRedactedThinking = "redacted_thinking"
)

// ContentBlock is one element of a message's content array.
type ContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"` // redacted_thinking

	// image / document
	Source *Source `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// cache_control and any other passthrough fields
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// Source is an image or document payload.
type Source struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Tool is a tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Type        string          `json:"type,omitempty"`
}

// MessagesResponse is the non-streaming POST /v1/messages result.
type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // always "message"
	Role         string         `json:"role"` // always "assistant"
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason,omitempty"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

// Usage is the token accounting block.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// Stop reasons.
const (
	StopEndTurn   = "end_turn"
	StopMaxTokens = "max_tokens"
	StopStopSeq   = "stop_sequence"
	StopToolUse   = "tool_use"
)

// APIError is the error envelope Claude Code expects on a non-2xx response.
// Claude Code classifies failures by this shape, so every error the gateway
// returns is emitted in it.
type APIError struct {
	Type  string `json:"type"` // always "error"
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// NewAPIError builds an error envelope.
func NewAPIError(kind, msg string) APIError {
	var e APIError
	e.Type = "error"
	e.Error.Type = kind
	e.Error.Message = msg
	return e
}

// ModelInfo is one entry of GET /v1/models.
//
// Claude Code's gateway discovery parses exactly these fields; display_name
// and description are nullable there.
type ModelInfo struct {
	Type        string `json:"type,omitempty"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// ModelList is the GET /v1/models envelope.
type ModelList struct {
	Data    []ModelInfo `json:"data"`
	HasMore bool        `json:"has_more"`
	FirstID *string     `json:"first_id"`
	LastID  *string     `json:"last_id"`
}
