// Package openai translates between the Anthropic Messages API and the OpenAI
// Chat Completions API, in both directions and for both streaming and
// non-streaming responses.
package openai

import "encoding/json"

// ChatRequest is POST {base_url}/chat/completions.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []ChatMsg `json:"messages"`

	// Exactly one of these carries the output cap; which one depends on the
	// provider. Reasoning models reject the legacy max_tokens.
	MaxTokens           int `json:"max_tokens,omitempty"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`

	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	StreamOptions   *StreamOptions  `json:"stream_options,omitempty"`
	Tools           []ChatTool      `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
}

// StreamOptions asks for a final usage frame on streamed responses.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMsg is one message in the OpenAI conversation.
//
// Content is a raw message because it is a string for assistant and tool
// messages but an array of parts for multimodal user messages.
type ChatMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`

	// Reasoning text, under the two spellings seen in the wild. Neither is
	// part of the official OpenAI schema.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Reasoning        string `json:"reasoning,omitempty"`
}

// ReasoningText returns whichever reasoning spelling the message used.
func (m ChatMsg) ReasoningText() string {
	if m.ReasoningContent != "" {
		return m.ReasoningContent
	}
	return m.Reasoning
}

// Roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ContentPart is one element of a multimodal user message.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL carries an inline data: URI or a remote URL.
type ImageURL struct {
	URL string `json:"url"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name and JSON-encoded arguments of a tool call.
type FunctionCall struct {
	Name string `json:"name,omitempty"`
	// Arguments is a JSON *string*, streamed in fragments.
	Arguments string `json:"arguments,omitempty"`
}

// ChatTool is a tool definition.
type ChatTool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the callable half of a ChatTool.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ChatResponse is a non-streaming completion.
type ChatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

// ChatChoice is one completion candidate.
type ChatChoice struct {
	Index        int     `json:"index"`
	Message      ChatMsg `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Delta        ChatMsg `json:"delta"`
}

// ChatUsage is OpenAI's token accounting.
type ChatUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetails breaks out the cached portion of the prompt.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// StreamChunk is one data: frame of a streamed completion.
type StreamChunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *ChatUsage     `json:"usage,omitempty"`
}

// StreamChoice is the per-chunk delta.
type StreamChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// ChunkDelta is the incremental payload of a streamed choice.
type ChunkDelta struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// Reasoning text, under the two spellings in the wild. Neither is part of
	// the official OpenAI schema; DeepSeek, vLLM, OpenRouter and others emit
	// one of them.
	ReasoningContent *string `json:"reasoning_content,omitempty"`
	Reasoning        *string `json:"reasoning,omitempty"`
}

// ReasoningText returns whichever reasoning spelling the chunk used.
func (d ChunkDelta) ReasoningText() string {
	if d.ReasoningContent != nil {
		return *d.ReasoningContent
	}
	if d.Reasoning != nil {
		return *d.Reasoning
	}
	return ""
}

// ErrorResponse is the OpenAI error envelope.
type ErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}
