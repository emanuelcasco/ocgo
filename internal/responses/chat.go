package responses

import "encoding/json"

// ChatRequest is the chat-completions request the bridge sends upstream. It is
// deliberately a separate type from the Anthropic-side request in cmd/ocgo:
// this package owns the Responses translation and must not depend on it.
type ChatRequest struct {
	Model             string          `json:"model"`
	Messages          []ChatMessage   `json:"messages"`
	Stream            bool            `json:"stream,omitempty"`
	StreamOptions     *StreamOptions  `json:"stream_options,omitempty"`
	MaxTokens         int             `json:"max_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Tools             []ChatTool      `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	// BuiltinTools names hosted tools the client asked for that chat
	// completions cannot serve (for example "web_search"). The caller decides
	// whether to translate them to another wire format or drop them.
	BuiltinTools []string `json:"-"`
	// Warnings records every item or tool that was skipped, so a session can be
	// diagnosed without re-reading a trace.
	Warnings []string `json:"-"`
}

// StreamOptions mirrors the OpenAI chat-completions stream_options object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMessage is one chat-completions message.
type ChatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

// ChatTool is a chat-completions function tool.
type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

// ChatFunction is the function object of a chat tool.
type ChatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ChatToolCall is one tool call on an assistant message.
type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ChatCallFunction `json:"function"`
}

// ChatCallFunction is the function object of a tool call.
type ChatCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// EmptyObjectSchema is the schema used for tools whose parameters are absent.
var EmptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// FreeformInputSchema is the schema used to expose a freeform (custom) tool,
// whose real argument grammar chat completions cannot express.
var FreeformInputSchema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"The tool input, verbatim."}},"required":["input"]}`)
