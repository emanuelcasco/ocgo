package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, body string) Request {
	t.Helper()
	req, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("Decode(%s): %v", body, err)
	}
	return req
}

func TestToChatMapsDeveloperToSystem(t *testing.T) {
	chat := decode(t, `{"input":[{"type":"message","role":"developer","content":"rules"},{"type":"message","role":"user","content":"hello"}]}`).ToChat()
	if len(chat.Messages) != 2 {
		t.Fatalf("got %d messages: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[0].Role != "system" || chat.Messages[0].Content != "rules" {
		t.Fatalf("developer message not mapped to system: %+v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "user" || chat.Messages[1].Content != "hello" {
		t.Fatalf("user message wrong: %+v", chat.Messages[1])
	}
}

func TestToChatPutsInstructionsFirst(t *testing.T) {
	chat := decode(t, `{"instructions":"be terse","input":[{"type":"message","role":"user","content":"hi"}]}`).ToChat()
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" || chat.Messages[0].Content != "be terse" {
		t.Fatalf("instructions should lead as a system message: %+v", chat.Messages)
	}
}

func TestToChatAcceptsAStringInput(t *testing.T) {
	chat := decode(t, `{"input":"hello"}`).ToChat()
	if len(chat.Messages) != 1 || chat.Messages[0].Content != "hello" {
		t.Fatalf("string input not handled: %+v", chat.Messages)
	}
}

func TestToChatPairsFunctionCallWithOutput(t *testing.T) {
	chat := decode(t, `{"input":[
		{"type":"message","role":"user","content":"run it"},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"/tmp"}
	]}`).ToChat()
	if len(chat.Messages) != 3 {
		t.Fatalf("got %d messages: %+v", len(chat.Messages), chat.Messages)
	}
	call := chat.Messages[1]
	if call.Role != "assistant" || len(call.ToolCalls) != 1 {
		t.Fatalf("function_call not mapped to assistant tool_calls: %+v", call)
	}
	if call.ToolCalls[0].ID != "call_1" || call.ToolCalls[0].Function.Name != "exec_command" {
		t.Fatalf("tool call wrong: %+v", call.ToolCalls[0])
	}
	result := chat.Messages[2]
	if result.Role != "tool" || result.ToolCallID != "call_1" || result.Content != "/tmp" {
		t.Fatalf("function_call_output not mapped to a tool message: %+v", result)
	}
}

func TestToChatGroupsParallelCallsIntoOneAssistantMessage(t *testing.T) {
	chat := decode(t, `{"input":[
		{"type":"function_call","call_id":"a","name":"one","arguments":"{}"},
		{"type":"function_call","call_id":"b","name":"two","arguments":"{}"},
		{"type":"function_call_output","call_id":"a","output":"1"},
		{"type":"function_call_output","call_id":"b","output":"2"}
	]}`).ToChat()
	if len(chat.Messages) != 3 {
		t.Fatalf("got %d messages: %+v", len(chat.Messages), chat.Messages)
	}
	if len(chat.Messages[0].ToolCalls) != 2 {
		t.Fatalf("parallel calls should share one assistant message: %+v", chat.Messages[0])
	}
}

func TestToChatDropsReasoningItems(t *testing.T) {
	chat := decode(t, `{"input":[
		{"type":"message","role":"user","content":"hi"},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}]},
		{"type":"message","role":"assistant","content":"hello"}
	]}`).ToChat()
	if len(chat.Messages) != 2 {
		t.Fatalf("reasoning item should be dropped, got %+v", chat.Messages)
	}
	for _, m := range chat.Messages {
		if strings.Contains(strings.ToLower(m.Role), "reason") {
			t.Fatalf("unexpected message: %+v", m)
		}
	}
}

func TestToChatWarnsOnUnknownItemType(t *testing.T) {
	chat := decode(t, `{"input":[{"type":"web_search_call","id":"ws_1"},{"type":"message","role":"user","content":"hi"}]}`).ToChat()
	if len(chat.Messages) != 1 {
		t.Fatalf("unknown item should be skipped, got %+v", chat.Messages)
	}
	if len(chat.Warnings) != 1 || !strings.Contains(chat.Warnings[0], "web_search_call") {
		t.Fatalf("expected one warning naming the type, got %v", chat.Warnings)
	}
}

func TestToChatMapsCustomToolCallToFunctionArguments(t *testing.T) {
	chat := decode(t, `{"input":[
		{"type":"custom_tool_call","id":"ctc_1","call_id":"call_9","name":"apply_patch","input":"*** Begin Patch\n"},
		{"type":"custom_tool_call_output","call_id":"call_9","output":"done"}
	]}`).ToChat()
	if len(chat.Messages) != 2 {
		t.Fatalf("got %d messages: %+v", len(chat.Messages), chat.Messages)
	}
	call := chat.Messages[0].ToolCalls[0]
	if call.ID != "call_9" || call.Function.Name != "apply_patch" {
		t.Fatalf("custom tool call wrong: %+v", call)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		t.Fatalf("custom tool arguments should be JSON: %v", err)
	}
	if args["input"] != "*** Begin Patch\n" {
		t.Fatalf("freeform input not preserved: %q", args["input"])
	}
	if chat.Messages[1].Role != "tool" || chat.Messages[1].ToolCallID != "call_9" {
		t.Fatalf("custom tool output wrong: %+v", chat.Messages[1])
	}
}

func TestToChatPreservesImages(t *testing.T) {
	chat := decode(t, `{"input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"describe this"},
		{"type":"input_image","image_url":"data:image/png;base64,abc","detail":"high"}
	]}]}`).ToChat()
	parts, ok := chat.Messages[0].Content.([]map[string]any)
	if !ok {
		t.Fatalf("expected content parts, got %T", chat.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts: %+v", len(parts), parts)
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "describe this" {
		t.Fatalf("text part wrong: %+v", parts[0])
	}
	image, ok := parts[1]["image_url"].(map[string]any)
	if !ok || image["url"] != "data:image/png;base64,abc" || image["detail"] != "high" {
		t.Fatalf("image part wrong: %+v", parts[1])
	}
}

func TestToChatTextOnlyPartsCollapseToString(t *testing.T) {
	chat := decode(t, `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}]}`).ToChat()
	if chat.Messages[0].Content != "ab" {
		t.Fatalf("text-only parts should collapse to a string, got %#v", chat.Messages[0].Content)
	}
}

func TestToChatRequestsStreamUsage(t *testing.T) {
	streaming := decode(t, `{"stream":true,"input":"hi"}`).ToChat()
	if streaming.StreamOptions == nil || !streaming.StreamOptions.IncludeUsage {
		t.Fatalf("streaming request should ask for usage: %+v", streaming.StreamOptions)
	}
	plain := decode(t, `{"input":"hi"}`).ToChat()
	if plain.StreamOptions != nil {
		t.Fatalf("non-streaming request should not set stream options: %+v", plain.StreamOptions)
	}
}

func TestChatToolsMapsEachType(t *testing.T) {
	chat := decode(t, `{"tools":[
		{"type":"function","name":"exec_command","parameters":{"type":"object"}},
		{"type":"custom","name":"apply_patch","description":"patch files","format":{"type":"grammar"}},
		{"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function","name":"close_agent"}]},
		{"type":"web_search"},
		{"type":"something_new","name":"mystery"}
	]}`).ToChat()

	var names []string
	for _, tool := range chat.Tools {
		names = append(names, tool.Function.Name)
	}
	if strings.Join(names, ",") != "exec_command,apply_patch" {
		t.Fatalf("chat tools = %v", names)
	}
	for _, tool := range chat.Tools {
		if tool.Type != "function" {
			t.Fatalf("tool type = %q, want function", tool.Type)
		}
	}
	if len(chat.BuiltinTools) != 1 || chat.BuiltinTools[0] != "web_search" {
		t.Fatalf("hosted tools = %v, want [web_search]", chat.BuiltinTools)
	}
	var warnings string
	for _, w := range chat.Warnings {
		warnings += w + "\n"
	}
	for _, want := range []string{"namespace", "multi_agent_v1", "something_new"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("warnings missing %q: %s", want, warnings)
		}
	}
}

func TestChatToolsExposesCustomToolAsFreeformInput(t *testing.T) {
	chat := decode(t, `{"tools":[{"type":"custom","name":"apply_patch","format":{"type":"grammar"}}]}`).ToChat()
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(chat.Tools[0].Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["input"].Type != "string" {
		t.Fatalf("custom tool should expose a string `input` parameter: %s", chat.Tools[0].Function.Parameters)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "input" {
		t.Fatalf("input should be required: %s", chat.Tools[0].Function.Parameters)
	}
}

func TestChatToolsSkipsNamelessFunction(t *testing.T) {
	chat := decode(t, `{"tools":[{"type":"function","name":""},{"type":"function","name":"keep"}]}`).ToChat()
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "keep" {
		t.Fatalf("chat tools = %+v", chat.Tools)
	}
	if len(chat.Warnings) != 1 {
		t.Fatalf("expected a warning for the nameless tool, got %v", chat.Warnings)
	}
}

func TestChatToolsDefaultsMissingParameters(t *testing.T) {
	chat := decode(t, `{"tools":[{"type":"function","name":"noargs"}]}`).ToChat()
	if string(chat.Tools[0].Function.Parameters) != string(EmptyObjectSchema) {
		t.Fatalf("parameters = %s, want the empty object schema", chat.Tools[0].Function.Parameters)
	}
}

func TestSanitizeInsertsPlaceholderForMissingResult(t *testing.T) {
	in := []ChatMessage{
		{Role: "user", Content: "run pwd"},
		{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "call_missing", Type: "function", Function: ChatCallFunction{Name: "exec_command"}}}},
		{Role: "user", Content: "next"},
	}
	out := sanitizeToolMessages(in)
	if len(out) != 4 {
		t.Fatalf("got %d messages: %+v", len(out), out)
	}
	if out[2].Role != "tool" || out[2].ToolCallID != "call_missing" || out[2].Content != unavailableToolResult {
		t.Fatalf("placeholder wrong: %+v", out[2])
	}
	if out[3].Role != "user" {
		t.Fatalf("following message was displaced: %+v", out[3])
	}
}

func TestSanitizeDropsOrphanToolMessage(t *testing.T) {
	in := []ChatMessage{
		{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "call_1", Type: "function", Function: ChatCallFunction{Name: "exec_command"}}}},
		{Role: "assistant", Content: "done"},
		{Role: "tool", ToolCallID: "call_1", Content: "late"},
	}
	out := sanitizeToolMessages(in)
	if len(out) != 3 {
		t.Fatalf("got %d messages: %+v", len(out), out)
	}
	if out[1].Role != "tool" || out[1].ToolCallID != "call_1" {
		t.Fatalf("missing result should be inserted before the next message: %+v", out)
	}
	if out[2].Role != "assistant" {
		t.Fatalf("late tool message was not dropped: %+v", out)
	}
}

func TestSanitizeDropsDuplicateResult(t *testing.T) {
	in := []ChatMessage{
		{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "call_1", Type: "function", Function: ChatCallFunction{Name: "exec_command"}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "first"},
		{Role: "tool", ToolCallID: "call_1", Content: "second"},
	}
	out := sanitizeToolMessages(in)
	if len(out) != 2 {
		t.Fatalf("got %d messages: %+v", len(out), out)
	}
	if out[1].Content != "first" {
		t.Fatalf("duplicate result kept the wrong one: %+v", out[1])
	}
}

func TestSanitizeLeavesValidTranscriptAlone(t *testing.T) {
	in := []ChatMessage{
		{Role: "user", Content: "run"},
		{Role: "assistant", ToolCalls: []ChatToolCall{
			{ID: "a", Type: "function", Function: ChatCallFunction{Name: "one"}},
			{ID: "b", Type: "function", Function: ChatCallFunction{Name: "two"}},
		}},
		{Role: "tool", ToolCallID: "a", Content: "1"},
		{Role: "tool", ToolCallID: "b", Content: "2"},
		{Role: "assistant", Content: "done"},
	}
	out := sanitizeToolMessages(in)
	if len(out) != len(in) {
		t.Fatalf("valid transcript changed length: %+v", out)
	}
	if &out[0] != &in[0] {
		t.Fatal("valid transcript should be returned without reallocating")
	}
}

func TestEstimateTokensIsNeverZero(t *testing.T) {
	for _, body := range []string{"", "{}", strings.Repeat("x", 1000)} {
		if got := EstimateTokens([]byte(body)); got < 1 {
			t.Fatalf("EstimateTokens(%d bytes) = %d, want >= 1", len(body), got)
		}
	}
}

func TestUsagePayloadOmitsZeroDetails(t *testing.T) {
	payload := Usage{Input: 10, Output: 5, Total: 15}.Payload()
	if _, ok := payload["input_tokens_details"]; ok {
		t.Fatal("zero cached tokens should be omitted")
	}
	full := Usage{Input: 10, Output: 5, Total: 15, Cached: 4, Reasoning: 2}.Payload()
	if _, ok := full["input_tokens_details"]; !ok {
		t.Fatal("cached tokens should be reported")
	}
	if _, ok := full["output_tokens_details"]; !ok {
		t.Fatal("reasoning tokens should be reported")
	}
}

func TestUsageFromJSONAcceptsBothShapes(t *testing.T) {
	openai := usageFromJSON([]byte(`{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":2}}`))
	if !openai.Present || openai.Input != 7 || openai.Output != 3 || openai.Total != 10 || openai.Cached != 2 {
		t.Fatalf("openai usage parsed wrong: %+v", openai)
	}
	alt := usageFromJSON([]byte(`{"input_tokens":4,"output_tokens":1}`))
	if !alt.Present || alt.Input != 4 || alt.Output != 1 || alt.Total != 5 {
		t.Fatalf("alternate usage parsed wrong: %+v", alt)
	}
	if got := usageFromJSON(nil); got.Present {
		t.Fatalf("absent usage should not be present: %+v", got)
	}
}
