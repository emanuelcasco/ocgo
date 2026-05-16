package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCodexProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := writeCodexProfile(path, "http://127.0.0.1:3456/v1/"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	for _, want := range []string{
		"[profiles.ocgo-launch]",
		`openai_base_url = "http://127.0.0.1:3456/v1/"`,
		`forced_login_method = "api"`,
		`model_provider = "ocgo-launch"`,
		`model_catalog_json = `,
		`model_reasoning_effort = "minimal"`,
		`model_reasoning_summary = "none"`,
		"[model_providers.ocgo-launch]",
		`name = "OpenCode Go"`,
		`base_url = "http://127.0.0.1:3456/v1/"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q in:\n%s", want, content)
		}
	}
}

func TestWriteCodexProfileReplacesExistingSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "[profiles.ocgo-launch]\nopenai_base_url = \"http://old/v1/\"\n\n[other]\nkey = \"value\"\n\n[model_providers.ocgo-launch]\nbase_url = \"http://old/v1/\"\n"
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexProfile(path, "http://new/v1/"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	content := string(b)
	if strings.Contains(content, "http://old") {
		t.Fatalf("old profile was not replaced:\n%s", content)
	}
	if strings.Count(content, "[profiles.ocgo-launch]") != 1 || strings.Count(content, "[model_providers.ocgo-launch]") != 1 {
		t.Fatalf("profile sections should be unique:\n%s", content)
	}
	if !strings.Contains(content, "[other]") || !strings.Contains(content, `key = "value"`) {
		t.Fatalf("unrelated section was not preserved:\n%s", content)
	}
}

func TestWriteCodexModelCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ocgo-models.json")
	if err := writeCodexModelCatalog(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	for _, want := range []string{`"models"`, `"slug": "deepseek-v4-pro"`, `"context_window": 128000`, `"truncation_policy"`, `"supports_image_detail_original": false`, `"image"`} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q in:\n%s", want, content)
		}
	}
}

func TestCodexModelCatalogAllowsImagesForKnownVisionModels(t *testing.T) {
	if !modelSupportsImages("kimi-k2.6") {
		t.Fatal("kimi-k2.6 should support image inputs")
	}
	if modelSupportsImages("deepseek-v4-pro") {
		t.Fatal("deepseek-v4-pro should not support image inputs")
	}
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{model: "kimi-k2.6", want: []string{"text", "image"}},
		{model: "deepseek-v4-pro", want: []string{"text"}},
	} {
		got := modelInputModalities(tc.model)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s modalities = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	if compareVersions("0.80.9", "0.81.0") >= 0 {
		t.Fatal("0.80.9 should be older")
	}
	if compareVersions("0.81.0", "0.81.0") != 0 {
		t.Fatal("same versions should compare equal")
	}
	if compareVersions("codex-cli", "0.81.0") >= 0 {
		t.Fatal("invalid version should compare as old")
	}
	if compareVersions("0.87.0", "0.81.0") <= 0 {
		t.Fatal("0.87.0 should be newer")
	}
}

func TestResponsesInputToMessages(t *testing.T) {
	messages := responsesInputToMessages([]byte(`[{"type":"message","role":"developer","content":"rules"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`))
	if len(messages) != 2 {
		t.Fatalf("got %d messages", len(messages))
	}
	if messages[0].Role != "system" || messages[0].Content != "rules" {
		t.Fatalf("bad developer conversion: %+v", messages[0])
	}
	if messages[1].Role != "user" || messages[1].Content != "hello" {
		t.Fatalf("bad user conversion: %+v", messages[1])
	}
}

func TestResponsesInputFunctionCallUsesCallID(t *testing.T) {
	messages := responsesInputToMessages([]byte(`[{"type":"function_call","id":"fc_123","call_id":"call_123","name":"shell","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_123","output":"/tmp"}]`))
	if len(messages) != 2 {
		t.Fatalf("got %d messages", len(messages))
	}
	if messages[0].ToolCalls[0].ID != "call_123" {
		t.Fatalf("tool call ID should match call_id for follow-up tool output: %+v", messages[0].ToolCalls[0])
	}
	if messages[0].ReasoningContent == "" {
		t.Fatalf("assistant tool call history should include fallback reasoning_content: %+v", messages[0])
	}
	if messages[1].ToolCallID != "call_123" {
		t.Fatalf("bad tool output ID: %+v", messages[1])
	}
}

func TestAnthropicToolUseHistoryIncludesFallbackReasoning(t *testing.T) {
	messages := contentToOpenAI(AMessage{Role: "assistant", Content: []byte(`[{"type":"tool_use","id":"call_123","name":"Bash","input":{"command":"pwd"}}]`)})
	if len(messages) != 1 {
		t.Fatalf("got %d messages", len(messages))
	}
	if messages[0].Role != "assistant" || len(messages[0].ToolCalls) != 1 {
		t.Fatalf("bad tool call conversion: %+v", messages[0])
	}
	if messages[0].ReasoningContent == "" {
		t.Fatalf("assistant tool call history should include fallback reasoning_content: %+v", messages[0])
	}
}

func TestAnthropicToolResultPreservesFollowingUserText(t *testing.T) {
	messages := contentToOpenAI(AMessage{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"call_123","content":"09:33:16"},{"type":"text","text":"https://figma.example/design what's going on here?"}]`)})
	if len(messages) != 2 {
		t.Fatalf("got %d messages: %+v", len(messages), messages)
	}
	if messages[0].Role != "tool" || messages[0].ToolCallID != "call_123" || messages[0].Content != "09:33:16" {
		t.Fatalf("bad tool result conversion: %+v", messages[0])
	}
	if messages[1].Role != "user" || !strings.Contains(contentString(messages[1].Content), "figma.example") {
		t.Fatalf("following user text was not preserved: %+v", messages[1])
	}
}

func TestResponsesInputPreservesImages(t *testing.T) {
	messages := responsesInputToMessages([]byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe this"},{"type":"input_image","image_url":"data:image/png;base64,abc","detail":"high"}]}]`))
	if len(messages) != 1 {
		t.Fatalf("got %d messages", len(messages))
	}
	parts, ok := messages[0].Content.([]OAIContentPart)
	if !ok {
		t.Fatalf("content should be multimodal parts: %+v", messages[0].Content)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "describe this" {
		t.Fatalf("bad text part: %+v", parts)
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,abc" || parts[1].ImageURL.Detail != "" {
		t.Fatalf("bad image part: %+v", parts[1])
	}
}

func TestResponsesImageKeepsKimiModel(t *testing.T) {
	req := ResponsesRequest{Model: "kimi-k2.6", Input: []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe this"},{"type":"input_image","image_url":"data:image/png;base64,abc"}]}]`)}
	out := responsesToChat(req)
	if out.Model != "kimi-k2.6" {
		t.Fatalf("image request should keep Kimi model, got %q", out.Model)
	}
	if err := validateImageSupport(out); err != nil {
		t.Fatalf("Kimi image request should validate: %v", err)
	}
}

func TestResponsesImageRejectsUnsupportedModel(t *testing.T) {
	req := ResponsesRequest{Model: "deepseek-v4-pro", Input: []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe this"},{"type":"input_image","image_url":"data:image/png;base64,abc"}]}]`)}
	out := responsesToChat(req)
	if err := validateImageSupport(out); err == nil || !strings.Contains(err.Error(), "deepseek-v4-pro") {
		t.Fatalf("DeepSeek image request should be rejected, got %v", err)
	}
}

func TestRawChatImageKeepsKimiAndStripsDetail(t *testing.T) {
	body, err := prepareChatBody([]byte(`{"model":"kimi-k2.6","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc","detail":"high"}}]}]}`))
	if err != nil {
		t.Fatalf("Kimi image request should validate: %v", err)
	}
	if !strings.Contains(string(body), `"model":"kimi-k2.6"`) {
		t.Fatalf("image chat body should keep Kimi model: %s", string(body))
	}
	if strings.Contains(string(body), `"detail"`) {
		t.Fatalf("image detail should be stripped for compatibility: %s", string(body))
	}
}

func TestRawChatImageRejectsUnsupportedModel(t *testing.T) {
	_, err := prepareChatBody([]byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "deepseek-v4-pro") {
		t.Fatalf("DeepSeek image request should be rejected, got %v", err)
	}
}

func TestAnthropicContentPreservesImages(t *testing.T) {
	messages := contentToOpenAI(AMessage{Role: "user", Content: []byte(`[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"abc"}}]`)})
	if len(messages) != 1 {
		t.Fatalf("got %d messages", len(messages))
	}
	parts, ok := messages[0].Content.([]OAIContentPart)
	if !ok {
		t.Fatalf("content should be multimodal parts: %+v", messages[0].Content)
	}
	if len(parts) != 2 || parts[0].Text != "what is this?" {
		t.Fatalf("bad text part: %+v", parts)
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/jpeg;base64,abc" {
		t.Fatalf("bad image part: %+v", parts[1])
	}
}

func TestAnthropicImageKeepsKimiModel(t *testing.T) {
	out := convertRequest(AnthropicRequest{Model: "kimi-k2.6", Messages: []AMessage{{Role: "user", Content: []byte(`[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]`)}}})
	if out.Model != "kimi-k2.6" {
		t.Fatalf("image request should keep Kimi model, got %q", out.Model)
	}
	if err := validateImageSupport(out); err != nil {
		t.Fatalf("Kimi image request should validate: %v", err)
	}
}

func TestAnthropicImageRejectsUnsupportedModel(t *testing.T) {
	out := convertRequest(AnthropicRequest{Model: "deepseek-v4-pro", Messages: []AMessage{{Role: "user", Content: []byte(`[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]`)}}})
	if err := validateImageSupport(out); err == nil || !strings.Contains(err.Error(), "deepseek-v4-pro") {
		t.Fatalf("DeepSeek image request should be rejected, got %v", err)
	}
}

func contentString(v any) string {
	s, _ := v.(string)
	return s
}

func TestParseWindowsNetstatPID(t *testing.T) {
	output := strings.Join([]string{
		"Proto  Local Address          Foreign Address        State           PID",
		"TCP    127.0.0.1:3456       0.0.0.0:0              LISTENING       4321",
		"TCP    [::1]:9999           [::]:0                 LISTENING       8765",
		"TCP    127.0.0.1:34560      0.0.0.0:0              LISTENING       1111",
	}, "\n")
	pid, err := parseWindowsNetstatPID(output, 3456)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 4321 {
		t.Fatalf("pid = %d, want 4321", pid)
	}
}

func TestParseWindowsNetstatPIDMatchesIPv6(t *testing.T) {
	output := "TCP    [::]:3456             [::]:0                 LISTENING       2468\n"
	pid, err := parseWindowsNetstatPID(output, 3456)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 2468 {
		t.Fatalf("pid = %d, want 2468", pid)
	}
}

func TestStreamAnthropicForwardsToolCalls(t *testing.T) {
	reasoningContentCache.Lock()
	reasoningContentCache.byCallID = map[string]string{}
	reasoningContentCache.Unlock()
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"Need pwd.","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	w := httptest.NewRecorder()
	streamAnthropic(w, body, "deepseek-v4-flash")
	out := w.Body.String()
	for _, want := range []string{
		`"type":"tool_use"`,
		`"name":"Bash"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"command\":"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	messages := responsesInputToMessages([]byte(`[{"type":"function_call","call_id":"call_abc","name":"Bash","arguments":"{\"command\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_abc","output":"/tmp"}]`))
	if messages[0].ReasoningContent != "Need pwd." {
		t.Fatalf("missing cached reasoning content: %+v", messages[0])
	}
}

func TestStreamResponsesForwardsToolCalls(t *testing.T) {
	reasoningContentCache.Lock()
	reasoningContentCache.byCallID = map[string]string{}
	reasoningContentCache.Unlock()
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"I should call the tool.","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"shell","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	w := httptest.NewRecorder()
	streamResponses(w, body, "deepseek-v4-flash")
	out := w.Body.String()
	for _, want := range []string{
		"event: response.output_item.added",
		`"type":"function_call"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"arguments":"{\"cmd\":\"pwd\"}"`,
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "response.output_text.delta") {
		t.Fatalf("tool-only stream should not emit text deltas:\n%s", out)
	}
	messages := responsesInputToMessages([]byte(`[{"type":"function_call","call_id":"call_abc","name":"shell","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_abc","output":"/tmp"}]`))
	if messages[0].ReasoningContent != "I should call the tool." {
		t.Fatalf("missing cached reasoning content: %+v", messages[0])
	}
}
