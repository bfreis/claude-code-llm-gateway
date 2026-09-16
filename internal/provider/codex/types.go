package codex

import "encoding/json"

// Request is the body POSTed to the Codex Responses endpoint
// (codex-rs/codex-api/src/common.rs ResponsesApiRequest).
type Request struct {
	Model             string     `json:"model"`
	Instructions      string     `json:"instructions,omitempty"`
	Input             []Item     `json:"input"`
	Tools             []Tool     `json:"tools,omitempty"`
	ToolChoice        string     `json:"tool_choice"`
	ParallelToolCalls bool       `json:"parallel_tool_calls"`
	Reasoning         *Reasoning `json:"reasoning,omitempty"`
	Store             bool       `json:"store"`
	Stream            bool       `json:"stream"`
	Include           []string   `json:"include"`
	ServiceTier       string     `json:"service_tier,omitempty"`
	// PromptCacheKey gives the backend cache affinity across turns of one
	// conversation. The Codex CLI derives it from the session id.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

// IncludeEncryptedReasoning is sent unconditionally by the Codex CLI.
const IncludeEncryptedReasoning = "reasoning.encrypted_content"

// Reasoning controls the reasoning effort and whether a summary comes back.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Item types on the Responses API. The wire uses a `type` discriminator with
// snake_case names (codex-rs/protocol/src/models.rs ResponseItem).
const (
	ItemMessage            = "message"
	ItemReasoning          = "reasoning"
	ItemFunctionCall       = "function_call"
	ItemFunctionCallOutput = "function_call_output"
)

// Content part types inside a message item (models.rs ContentItem).
const (
	PartInputText  = "input_text"
	PartInputImage = "input_image"
	PartOutputText = "output_text"
)

// Roles on the Responses API.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// Item is one element of the `input` array.
//
// The four variants this gateway produces are distinguished by Type; fields
// belonging to other variants stay empty and are omitted.
type Item struct {
	Type string `json:"type"`

	// message
	Role    string `json:"role,omitempty"`
	Content []Part `json:"content,omitempty"`

	// function_call
	Name string `json:"name,omitempty"`
	// Arguments is a JSON-encoded *string*, not an object: "The Responses API
	// returns the function call arguments as a string that contains JSON, not
	// as an already-parsed object" (models.rs).
	Arguments string `json:"arguments,omitempty"`

	// function_call and function_call_output
	CallID string `json:"call_id,omitempty"`

	// function_call_output. A bare JSON string for text results; the array
	// form exists for image results, which this gateway does not produce.
	Output json.RawMessage `json:"output,omitempty"`

	// reasoning
	EncryptedContent string `json:"encrypted_content,omitempty"`
	Summary          []Part `json:"summary,omitempty"`

	// ID is echoed back when replaying an item the backend produced.
	ID string `json:"id,omitempty"`
}

// Part is one content element of a message item.
type Part struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// ImageURL carries a data: URI for input_image.
	ImageURL string `json:"image_url,omitempty"`
}

// Tool is a function tool definition.
//
// The shape is flat — `{"type":"function","name":…}` — with no nested
// `function` object, because serde's `tag = "type"` on a newtype variant
// flattens the inner struct (codex-rs/tools/src/tool_spec.rs,
// codex-rs/tools/src/responses_api.rs ResponsesApiTool).
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Strict      bool            `json:"strict"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolTypeFunction is the only tool type this gateway emits.
const ToolTypeFunction = "function"
