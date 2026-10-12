// Package responses translates the OpenAI Responses API, which Codex CLI
// speaks, to and from chat completions, which OpenCode Go serves.
//
// Request translation is in ToChat; response translation is the streaming state
// machine in stream.go.
package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request is the subset of a Responses request the bridge acts on. Unknown
// top-level fields are ignored rather than rejected.
type Request struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	Stream             bool            `json:"stream"`
	MaxOutputTokens    int             `json:"max_output_tokens"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	Tools              []Tool          `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	PromptCacheKey     string          `json:"prompt_cache_key"`
	Store              *bool           `json:"store"`
	PreviousResponseID string          `json:"previous_response_id"`
	Include            []string        `json:"include"`
	Reasoning          json.RawMessage `json:"reasoning"`

	// Reasoning-effort aliases accepted from different clients. The caller
	// resolves these, because the mapping already lives in cmd/ocgo.
	Thinking        json.RawMessage `json:"thinking"`
	ReasoningEffort json.RawMessage `json:"reasoning_effort"`
	Effort          json.RawMessage `json:"effort"`
	Level           json.RawMessage `json:"level"`
	Depth           json.RawMessage `json:"depth"`
	OutputConfig    json.RawMessage `json:"output_config"`
}

// Decode parses a Responses request body.
func Decode(raw []byte) (Request, error) {
	var r Request
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("invalid responses request: %w", err)
	}
	return r, nil
}

// ToChat converts the request into a chat-completions request. The model and
// reasoning effort are left for the caller to set, because both depend on
// ocgo's own model mapping and effort heuristics.
//
// Every item type that cannot be represented is recorded in Warnings and
// skipped; translation never fails on an unknown item.
func (r Request) ToChat() ChatRequest {
	out := ChatRequest{
		Stream:            r.Stream,
		MaxTokens:         r.MaxOutputTokens,
		Temperature:       r.Temperature,
		TopP:              r.TopP,
		ToolChoice:        r.ToolChoice,
		ParallelToolCalls: r.ParallelToolCalls,
	}
	if r.Stream {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if strings.TrimSpace(r.Instructions) != "" {
		out.Messages = append(out.Messages, ChatMessage{Role: "system", Content: r.Instructions})
	}
	messages, warnings := inputToMessages(r.Input)
	out.Warnings = append(out.Warnings, warnings...)
	out.Messages = append(out.Messages, sanitizeToolMessages(messages)...)
	out.Tools, out.BuiltinTools, warnings = chatTools(r.Tools)
	out.Warnings = append(out.Warnings, warnings...)
	return out
}

// inputToMessages converts the Responses `input` field into chat messages.
func inputToMessages(raw json.RawMessage) ([]ChatMessage, []string) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []ChatMessage{{Role: "user", Content: s}}, nil
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return []ChatMessage{{Role: "user", Content: string(raw)}}, nil
	}

	var out []ChatMessage
	var warnings []string
	var pending []ChatToolCall
	flush := func() {
		if len(pending) > 0 {
			out = append(out, ChatMessage{Role: "assistant", ToolCalls: pending})
			pending = nil
		}
	}

	for _, item := range items {
		var typ, role string
		_ = json.Unmarshal(item["type"], &typ)
		_ = json.Unmarshal(item["role"], &role)
		switch typ {
		case "message", "":
			flush()
			switch role {
			case "developer":
				role = "system"
			case "":
				role = "user"
			}
			out = append(out, ChatMessage{Role: role, Content: contentValue(item["content"])})
		case "function_call":
			var id, callID, name, args string
			_ = json.Unmarshal(item["id"], &id)
			_ = json.Unmarshal(item["call_id"], &callID)
			_ = json.Unmarshal(item["name"], &name)
			_ = json.Unmarshal(item["arguments"], &args)
			if callID == "" {
				callID = id
			}
			pending = append(pending, ChatToolCall{ID: callID, Type: "function", Function: ChatCallFunction{Name: name, Arguments: args}})
		case "function_call_output":
			flush()
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			out = append(out, ChatMessage{Role: "tool", ToolCallID: callID, Content: contentText(item["output"])})
		case "reasoning":
			// Reasoning items are not sent upstream: chat completions has no
			// equivalent field, and dropping them preserves the order of
			// everything else.
		case "custom_tool_call":
			// A custom tool call is carried as a function call whose arguments
			// are the verbatim freeform input.
			var id, callID, name, input string
			_ = json.Unmarshal(item["id"], &id)
			_ = json.Unmarshal(item["call_id"], &callID)
			_ = json.Unmarshal(item["name"], &name)
			_ = json.Unmarshal(item["input"], &input)
			if callID == "" {
				callID = id
			}
			encoded, _ := json.Marshal(map[string]string{"input": input})
			pending = append(pending, ChatToolCall{ID: callID, Type: "function", Function: ChatCallFunction{Name: name, Arguments: string(encoded)}})
		case "custom_tool_call_output":
			flush()
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			out = append(out, ChatMessage{Role: "tool", ToolCallID: callID, Content: contentText(item["output"])})
		default:
			warnings = append(warnings, "skipped input item of type "+typ)
		}
	}
	flush()
	return out, warnings
}

// contentValue renders a Responses content field as either a plain string or a
// chat content-part array, matching what chat completions accepts.
func contentValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var text strings.Builder
	var out []map[string]any
	allText := true
	for _, part := range parts {
		var typ string
		_ = json.Unmarshal(part["type"], &typ)
		switch typ {
		case "input_text", "output_text", "text", "summary_text":
			t := contentText(part["text"])
			text.WriteString(t)
			out = append(out, map[string]any{"type": "text", "text": t})
		case "input_image":
			allText = false
			out = append(out, map[string]any{"type": "image_url", "image_url": imageURL(part)})
		case "refusal":
			t := contentText(part["refusal"])
			text.WriteString(t)
			out = append(out, map[string]any{"type": "text", "text": t})
		default:
			// Unknown part types are dropped rather than sent upstream.
			allText = false
		}
	}
	if allText {
		return text.String()
	}
	return out
}

// imageURL renders a Responses input_image part as a chat image_url object.
func imageURL(part map[string]json.RawMessage) map[string]any {
	var url string
	var detail string
	if json.Unmarshal(part["image_url"], &url) != nil {
		var nested struct {
			URL    string `json:"url"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(part["image_url"], &nested) == nil {
			url, detail = nested.URL, nested.Detail
		}
	}
	_ = json.Unmarshal(part["detail"], &detail)
	out := map[string]any{"url": url}
	if detail != "" {
		out["detail"] = detail
	}
	return out
}

// contentText renders any content field as text.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var sb strings.Builder
	for _, part := range parts {
		for _, key := range []string{"text", "output", "content"} {
			if v, ok := part[key]; ok {
				sb.WriteString(contentText(v))
			}
		}
	}
	return sb.String()
}

const unavailableToolResult = "Tool result unavailable."

// sanitizeToolMessages enforces the chat-completions invariant that an
// assistant message with tool_calls is immediately followed by tool messages
// for exactly those call IDs. Orphan tool messages are dropped and missing
// results get a placeholder, so an upstream model never sees an invalid
// transcript. A transcript that already satisfies the invariant is returned
// unchanged, without allocating.
func sanitizeToolMessages(messages []ChatMessage) []ChatMessage {
	if toolMessagesValid(messages) {
		return messages
	}
	out := make([]ChatMessage, 0, len(messages))
	// Explicit index: the body consumes the run of tool messages that follows
	// each assistant message, which range-over-slice cannot express.
	for i := 0; i < len(messages); {
		m := messages[i]
		i++
		if m.Role == "tool" {
			// Keep only results for the assistant message immediately before,
			// and only the first result per call id.
			if len(out) == 0 || len(out[len(out)-1].ToolCalls) == 0 || containsID(out, m.ToolCallID) {
				continue
			}
			out = append(out, m)
			continue
		}
		out = append(out, m)
		if len(m.ToolCalls) == 0 {
			continue
		}
		var seen []string
		for i < len(messages) && messages[i].Role == "tool" {
			t := messages[i]
			i++
			if containsString(seen, t.ToolCallID) {
				continue
			}
			seen = append(seen, t.ToolCallID)
			out = append(out, t)
		}
		for _, call := range m.ToolCalls {
			if !containsString(seen, call.ID) {
				out = append(out, ChatMessage{Role: "tool", ToolCallID: call.ID, Content: unavailableToolResult})
			}
		}
	}
	return out
}

// toolMessagesValid reports whether the transcript already satisfies the
// assistant-tool_calls / tool-result invariant.
func toolMessagesValid(messages []ChatMessage) bool {
	for i := 0; i < len(messages); {
		m := messages[i]
		if m.Role == "tool" {
			return false
		}
		i++
		if len(m.ToolCalls) == 0 {
			continue
		}
		var seen []string
		for i < len(messages) && messages[i].Role == "tool" {
			id := messages[i].ToolCallID
			if containsString(seen, id) {
				return false
			}
			seen = append(seen, id)
			i++
		}
		for _, call := range m.ToolCalls {
			if !containsString(seen, call.ID) {
				return false
			}
		}
	}
	return true
}

// containsID reports whether the trailing run of tool messages in out already
// carries a result for id.
func containsID(out []ChatMessage, id string) bool {
	for i := len(out) - 1; i >= 0 && out[i].Role == "tool"; i-- {
		if out[i].ToolCallID == id {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
