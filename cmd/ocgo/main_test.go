package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ocgo/internal/responses"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ocgo-test-*")
	if err != nil {
		panic(err)
	}
	old := modelMappingFile
	oldRemoteModels := remoteModels
	oldOfficialModels := officialModels
	modelMappingFile = func() string { return filepath.Join(dir, "model-mapping.json") }
	remoteModels = newLazyFetcher(func() (map[string]remoteModelInfo, error) {
		return nil, errors.New("remote model fetch disabled in tests")
	})
	officialModels = newLazyFetcher(func() ([]string, error) { return nil, errors.New("official model fetch disabled in tests") })
	code := m.Run()
	modelMappingFile = old
	remoteModels = oldRemoteModels
	officialModels = oldOfficialModels
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestWriteCodexProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := writeCodexProfile(path, "http://127.0.0.1:3456/v1/"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ocgo-launch.config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	for _, want := range []string{
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
	if strings.Contains(content, "[profiles.ocgo-launch]") {
		t.Fatalf("new Codex profile file must not contain legacy [profiles] table:\n%s", content)
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "" {
		t.Fatalf("root Codex config should not contain ocgo legacy profile entries:\n%s", string(b))
	}
}

func TestWriteCodexProfileMigratesLegacySections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	existing := "profile = \"ocgo-launch\"\nkeep = \"top\"\n\n[profiles.ocgo-launch]\nopenai_base_url = \"http://old/v1/\"\n\n[profiles.ocgo-launch.features]\nmemories = false\n\n[other]\nkey = \"value\"\n\n[model_providers.ocgo-launch]\nbase_url = \"http://old/v1/\"\n\n[model_providers.ocgo-launch.headers]\nfoo = \"bar\"\n"
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexProfile(path, "http://new/v1/"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	content := string(b)
	for _, gone := range []string{"http://old", `profile = "ocgo-launch"`} {
		if strings.Contains(content, gone) {
			t.Fatalf("legacy Codex profile config %q was not removed:\n%s", gone, content)
		}
	}
	for _, gone := range []string{"[profiles.ocgo-launch]", "[profiles.ocgo-launch.features]", "[model_providers.ocgo-launch]", "[model_providers.ocgo-launch.headers]", `openai_base_url = "http://new/v1/"`} {
		if strings.Contains(content, gone) {
			t.Fatalf("legacy Codex profile config %q was re-added:\n%s", gone, content)
		}
	}
	if !strings.Contains(content, `keep = "top"`) || !strings.Contains(content, "[other]") || !strings.Contains(content, `key = "value"`) {
		t.Fatalf("unrelated section was not preserved:\n%s", content)
	}
	profile, _ := os.ReadFile(filepath.Join(dir, "ocgo-launch.config.toml"))
	if !strings.Contains(string(profile), `openai_base_url = "http://new/v1/"`) || !strings.Contains(string(profile), "[model_providers.ocgo-launch]") {
		t.Fatalf("new profile file was not written correctly:\n%s", string(profile))
	}
}

func TestWriteCodexModelCatalog(t *testing.T) {
	withTempModelMappingFile(t, filepath.Join(t.TempDir(), "model-mapping.json"))
	path := filepath.Join(t.TempDir(), "ocgo-models.json")
	if err := writeCodexModelCatalog(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	for _, want := range []string{`"models"`, `"slug": "deepseek-v4-pro"`, `"slug": "qwen3.7-max"`, `"slug": "minimax-m3"`, `"context_window": 128000`, `"truncation_policy"`, `"supports_image_detail_original": false`, `"image"`} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q in:\n%s", want, content)
		}
	}
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(b, &catalog); err != nil {
		t.Fatal(err)
	}
	var minimax map[string]any
	var qwen map[string]any
	for _, model := range catalog.Models {
		switch model["slug"] {
		case "minimax-m3":
			minimax = model
		case "qwen3.7-max":
			qwen = model
		}
	}
	if minimax == nil {
		t.Fatal("minimax-m3 not found in catalog")
	}
	if got := int(minimax["context_window"].(float64)); got != 512000 {
		t.Fatalf("minimax-m3 context_window = %d, want 512000", got)
	}
	if got := minimax["display_name"]; got != "MiniMax M3" {
		t.Fatalf("minimax-m3 display_name = %v, want MiniMax M3", got)
	}
	modalities := fmt.Sprint(minimax["input_modalities"])
	for _, want := range []string{"text", "image"} {
		if !strings.Contains(modalities, want) {
			t.Fatalf("minimax-m3 modalities missing %s: %v", want, minimax["input_modalities"])
		}
	}
	if strings.Contains(modalities, "video") {
		t.Fatalf("Codex catalog modalities must not include unsupported video modality: %v", minimax["input_modalities"])
	}
	if qwen == nil {
		t.Fatal("qwen3.7-max not found in catalog")
	}
	if got := qwen["supports_search_tool"]; got != true {
		t.Fatalf("qwen3.7-max supports_search_tool = %v, want true", got)
	}
}

func TestModelMappingsLoadSaveAndResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	withTempModelMappingFile(t, path)

	m, err := loadModelMappings()
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveMappedModel("claude", "claude-sonnet-4-5", m); got != "claude-sonnet-4-5" {
		t.Fatalf("unconfigured claude model should pass through, got %q", got)
	}
	m["claude"]["claude-sonnet"] = "kimi-k2.6"
	m["claude"]["claude-sonnet-4-5"] = "qwen3.7-max"
	m["codex"]["gpt-5"] = "deepseek-v4-pro"
	if err := saveModelMappings(m); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadModelMappings()
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveMappedModel("claude", "claude-sonnet-4-5", reloaded); got != "qwen3.7-max" {
		t.Fatalf("custom claude mapping = %q", got)
	}
	if got := resolveMappedModel("codex", "gpt-5", reloaded); got != "deepseek-v4-pro" {
		t.Fatalf("custom codex mapping = %q", got)
	}
}

func TestPrepareChatBodyAppliesCodexMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	withTempModelMappingFile(t, path)
	m := defaultModelMappings()
	m["codex"]["gpt-5"] = "deepseek-v4-pro"
	if err := saveModelMappings(m); err != nil {
		t.Fatal(err)
	}
	body, err := prepareChatBody([]byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"model":"deepseek-v4-pro"`) {
		t.Fatalf("mapping was not applied: %s", string(body))
	}
}

func TestMappingUnsetCommandRemovesMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	withTempModelMappingFile(t, path)
	m := defaultModelMappings()
	m["codex"]["gpt-5.5"] = "deepseek-v4-pro"
	if err := saveModelMappings(m); err != nil {
		t.Fatal(err)
	}
	cmd := toolMappingCmd("codex")
	cmd.SetArgs([]string{"unset", "gpt-5.5"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadModelMappings()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded["codex"]["gpt-5.5"]; ok {
		t.Fatalf("mapping was not removed: %+v", reloaded["codex"])
	}
}

func withTempModelMappingFile(t *testing.T, path string) {
	t.Helper()
	old := modelMappingFile
	modelMappingFile = func() string { return path }
	t.Cleanup(func() { modelMappingFile = old })
}

func withModelFetchers(t *testing.T, remote map[string]remoteModelInfo, official []string) {
	t.Helper()
	oldRemoteModels := remoteModels
	oldOfficialModels := officialModels
	remoteModels = newLazyFetcher(func() (map[string]remoteModelInfo, error) {
		if remote == nil {
			return nil, errors.New("remote unavailable")
		}
		return remote, nil
	})
	officialModels = newLazyFetcher(func() ([]string, error) {
		if official == nil {
			return nil, errors.New("official unavailable")
		}
		return official, nil
	})
	t.Cleanup(func() {
		remoteModels = oldRemoteModels
		officialModels = oldOfficialModels
	})
}

func testRemoteModel(name string, contextWindow int, modalities ...string) remoteModelInfo {
	var m remoteModelInfo
	m.Name = name
	m.Limit.Context = contextWindow
	m.Modalities.Input = append([]string(nil), modalities...)
	return m
}

func TestKnownModelIDsPreferOfficialThenRemoteThenFallback(t *testing.T) {
	withModelFetchers(t, map[string]remoteModelInfo{"remote-b": {}, "remote-a": {}}, []string{"official-b", "official-a"})
	if got := strings.Join(knownModelIDs(), ","); got != "official-a,official-b" {
		t.Fatalf("official IDs = %s", got)
	}

	withModelFetchers(t, map[string]remoteModelInfo{"remote-b": {}, "remote-a": {}}, nil)
	if got := strings.Join(knownModelIDs(), ","); got != "remote-a,remote-b" {
		t.Fatalf("remote fallback IDs = %s", got)
	}

	withModelFetchers(t, nil, nil)
	if got := knownModelIDs(); len(got) == 0 || got[0] != fallbackModelIDs[0] {
		t.Fatalf("hardcoded fallback IDs = %+v", got)
	}
}

func TestModelMetadataUsesRemoteFixtureWithoutLiveNetwork(t *testing.T) {
	withModelFetchers(t, map[string]remoteModelInfo{
		"kimi-k2.6": testRemoteModel("Kimi K2.6", 262144, "text", "image", "video"),
	}, nil)
	meta := modelMetadata("kimi-k2.6")
	if meta.DisplayName != "Kimi K2.6" || meta.ContextWindow != 262144 {
		t.Fatalf("remote metadata was not applied: %+v", meta)
	}
	if strings.Join(meta.InputModalities, ",") != "text,image,video" {
		t.Fatalf("input modalities = %+v", meta.InputModalities)
	}
	if strings.Join(meta.CodexInputModalities, ",") != "text,image" {
		t.Fatalf("codex modalities should exclude unsupported video: %+v", meta.CodexInputModalities)
	}
}

func TestCodexModelCatalogAllowsImagesForKnownVisionModels(t *testing.T) {
	if !modelSupportsImages("kimi-k2.6") {
		t.Fatal("kimi-k2.6 should support image inputs")
	}
	if !modelSupportsImages("minimax-m3") {
		t.Fatal("minimax-m3 should support image inputs")
	}
	if modelSupportsImages("deepseek-v4-pro") {
		t.Fatal("deepseek-v4-pro should not support image inputs")
	}
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{model: "kimi-k2.6", want: []string{"text", "image"}},
		{model: "minimax-m3", want: []string{"text", "image", "video"}},
		{model: "deepseek-v4-pro", want: []string{"text"}},
	} {
		got := modelInputModalities(tc.model)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s modalities = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

func TestAnthropicEndpointModels(t *testing.T) {
	for _, model := range []string{"qwen3.7-max", "minimax-m3", "minimax-m2.7", "opencode-go/qwen3.7-max", "opencode-go/minimax-m3"} {
		if !modelUsesAnthropicEndpoint(model) {
			t.Fatalf("%s should use Anthropic-compatible upstream", model)
		}
	}
	for _, model := range []string{"kimi-k2.6", "qwen3.6-plus", "qwen3.5-plus"} {
		if modelUsesAnthropicEndpoint(model) {
			t.Fatalf("%s should use OpenAI-compatible upstream", model)
		}
	}
}

func TestChatToAnthropicForCodexModel(t *testing.T) {
	req, err := responses.Decode([]byte(`{"model":"qwen3.7-max","stream":true,"input":[{"type":"message","role":"user","content":"hello"}],"tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	chat := req.ToChat()
	chat.Model = req.Model
	ar := chatToAnthropic(chatToOAI(chat))
	if ar.Model != "qwen3.7-max" || !ar.Stream || ar.MaxTokens == 0 {
		t.Fatalf("bad anthropic request metadata: %+v", ar)
	}
	if len(ar.Messages) != 1 || ar.Messages[0].Role != "user" || string(ar.Messages[0].Content) != `"hello"` {
		t.Fatalf("bad anthropic messages: %+v", ar.Messages)
	}
	if len(ar.Tools) != 1 || ar.Tools[0].Name != "shell" {
		t.Fatalf("bad anthropic tools: %+v", ar.Tools)
	}
}

func TestNormalizeAnthropicRequestForStrictUpstream(t *testing.T) {
	ar := AnthropicRequest{
		Model:        "opencode-go/qwen3.7-max",
		Thinking:     []byte(`{"type":"enabled","budget_tokens":1024}`),
		Reasoning:    []byte(`{"effort":"high"}`),
		OutputConfig: []byte(`{"reasoning":{"depth":2}}`),
		System:       []byte(`[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}]`),
		Tools:        []ATool{{Type: "web_search_20250305", Name: "web_search", MaxUses: 8, AllowedDomains: []string{"example.com"}}},
		Messages: []AMessage{{Role: "user", Content: []byte(`[
			{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}},
			{"type":"thinking","thinking":"private","signature":"abc"},
			{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]}
		]`)}},
	}
	normalizeAnthropicRequestForUpstream(&ar)
	body, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	for _, gone := range []string{"opencode-go/", "thinking", "reasoning", "output_config", "cache_control", "signature"} {
		if strings.Contains(out, gone) {
			t.Fatalf("strict upstream request still contains %q: %s", gone, out)
		}
	}
	for _, want := range []string{`"model":"qwen3.7-max"`, `"system":"rules"`, `"type":"text"`, `"text":"hello"`, `"tool_use_id":"toolu_1"`, `"type":"web_search_20250305"`, `"name":"web_search"`, `"max_uses":8`, `"allowed_domains":["example.com"]`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in normalized request: %s", want, out)
		}
	}
}

func TestNormalizeAnthropicToolResultTruncatesLargeFetchContent(t *testing.T) {
	large := strings.Repeat("a", maxAnthropicToolResultContentChars+50) + "tail-should-be-omitted"
	ar := AnthropicRequest{
		Model: "qwen3.7-max",
		Messages: []AMessage{{Role: "user", Content: marshalJSON([]map[string]any{{
			"type":        "tool_result",
			"tool_use_id": "toolu_fetch",
			"content":     []map[string]any{{"type": "text", "text": large}},
		}})}},
	}

	normalizeAnthropicRequestForUpstream(&ar)
	body, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	if strings.Contains(out, "tail-should-be-omitted") {
		t.Fatalf("large fetched content was not truncated: %s", out[len(out)-200:])
	}
	for _, want := range []string{`"tool_use_id":"toolu_fetch"`, "ocgo truncated tool_result content"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in normalized request: %s", want, out)
		}
	}
}

func TestNormalizeQwenAnthropicRequestThinkingVariants(t *testing.T) {
	zero := 0.0
	topP := 0.8
	for _, tc := range []struct {
		name string
		req  AnthropicRequest
	}{
		{
			name: "thinking enabled with budget",
			req: AnthropicRequest{
				Thinking: []byte(`{"type":"enabled","budget_tokens":2048}`),
			},
		},
		{
			name: "thinking disabled with zero temperature",
			req: AnthropicRequest{
				Thinking:    []byte(`{"type":"disabled"}`),
				Temperature: &zero,
			},
		},
		{
			name: "reasoning effort high",
			req: AnthropicRequest{
				ReasoningEffort: []byte(`"high"`),
				TopP:            &topP,
			},
		},
		{
			name: "nested output config reasoning",
			req: AnthropicRequest{
				OutputConfig: []byte(`{"reasoning":{"effort":"medium"}}`),
			},
		},
		{
			name: "legacy effort level depth fields",
			req: AnthropicRequest{
				Effort: []byte(`"low"`),
				Level:  []byte(`2`),
				Depth:  []byte(`{"level":"high"}`),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ar := tc.req
			ar.Model = "opencode-go/qwen3.7-max"
			ar.Stream = true
			ar.MaxTokens = 1234
			ar.System = []byte(`plain rules`)
			ar.Tools = []ATool{{Name: "Bash", Description: "run command", InputSchema: []byte(`{"type":"object","properties":{"command":{"type":"string"}}}`)}}
			ar.Messages = []AMessage{{Role: "user", Content: []byte(`[
				{"type":"text","text":"hello qwen","cache_control":{"type":"ephemeral"}},
				{"type":"thinking","thinking":"hidden chain of thought","signature":"sig_123"},
				{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"pwd","cache_control":{"type":"ephemeral"}}}
			]`)}}

			normalizeAnthropicRequestForUpstream(&ar)
			body, err := json.Marshal(ar)
			if err != nil {
				t.Fatal(err)
			}
			out := string(body)
			for _, gone := range []string{"opencode-go/", "thinking", "reasoning", "reasoning_effort", "output_config", "effort", "level", "depth", "cache_control", "signature", "hidden chain of thought"} {
				if strings.Contains(out, gone) {
					t.Fatalf("normalized qwen request still contains %q: %s", gone, out)
				}
			}
			for _, want := range []string{`"model":"qwen3.7-max"`, `"stream":true`, `"max_tokens":1234`, `"system":"plain rules"`, `"name":"Bash"`, `"id":"toolu_1"`, `"command":"pwd"`, `"text":"hello qwen"`} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in normalized qwen request: %s", want, out)
				}
			}
			if tc.req.Temperature != nil && !strings.Contains(out, `"temperature":0`) {
				t.Fatalf("temperature option was not preserved: %s", out)
			}
			if tc.req.TopP != nil && !strings.Contains(out, `"top_p":0.8`) {
				t.Fatalf("top_p option was not preserved: %s", out)
			}
		})
	}
}

func TestForwardAnthropicSendsNormalizedBody(t *testing.T) {
	var forwarded string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Fatalf("missing API key header: %q", r.Header.Get("X-API-Key"))
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		forwarded = string(b)
		for _, gone := range []string{"opencode-go/", "thinking", "reasoning", "cache_control", "signature"} {
			if strings.Contains(forwarded, gone) {
				t.Fatalf("forwarded body still contains %q: %s", gone, forwarded)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	oldURL := anthropicURL
	anthropicURL = srv.URL
	defer func() { anthropicURL = oldURL }()

	resp, err := forwardAnthropic(context.Background(), Config{APIKey: "test-key"}, AnthropicRequest{
		Model:     "opencode-go/qwen3.7-max",
		Thinking:  []byte(`{"type":"enabled"}`),
		System:    []byte(`[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}]`),
		Messages:  []AMessage{{Role: "user", Content: []byte(`[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}},{"type":"thinking","thinking":"private","signature":"abc"}]`)}},
		MaxTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{`"model":"qwen3.7-max"`, `"system":"rules"`, `"messages"`, `"text":"hello"`} {
		if !strings.Contains(forwarded, want) {
			t.Fatalf("missing %q in forwarded body: %s", want, forwarded)
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

func TestRawChatStreamRequestsUsage(t *testing.T) {
	body, err := prepareChatBody([]byte(`{"model":"kimi-k2.6","stream":true,"stream_options":{"foo":"bar"},"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	options, ok := req["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("missing stream options in %s", string(body))
	}
	if options["include_usage"] != true || options["foo"] != "bar" {
		t.Fatalf("bad stream options: %+v", options)
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{in: "minimal", want: "minimal"},
		{in: "0", want: "minimal"},
		{in: "low", want: "low"},
		{in: "1", want: "low"},
		{in: "medium", want: "medium"},
		{in: "2", want: "medium"},
		{in: "high", want: "high"},
		{in: "xhigh", want: "high"},
		{in: "max", want: "high"},
	} {
		if got := normalizeReasoningEffort(tc.in); got != tc.want {
			t.Fatalf("normalizeReasoningEffort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRawChatReasoningEffortPassThrough(t *testing.T) {
	body, err := prepareChatBody([]byte(`{"model":"glm-5.1","reasoning":{"effort":"xhigh"},"thinking":{"type":"enabled"},"output_config":{"reasoning":{"depth":2}},"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high in %s", req["reasoning_effort"], string(body))
	}
	for _, key := range []string{"reasoning", "thinking", "effort", "level", "depth", "output_config"} {
		if _, ok := req[key]; ok {
			t.Fatalf("%s should be stripped from forwarded chat body: %s", key, string(body))
		}
	}
}

func TestReasoningEffortExtraction(t *testing.T) {
	for _, tc := range []struct {
		raw  json.RawMessage
		want string
	}{
		{raw: []byte(`"low"`), want: "low"},
		{raw: []byte(`3`), want: "high"},
		{raw: []byte(`{"level":"medium"}`), want: "medium"},
		{raw: []byte(`{"type":"enabled"}`), want: "high"},
		{raw: []byte(`{"reasoning":{"depth":1}}`), want: "low"},
	} {
		if got := downstreamReasoningEffort(tc.raw); got != tc.want {
			t.Fatalf("downstreamReasoningEffort(%s) = %q, want %q", string(tc.raw), got, tc.want)
		}
	}
}

func TestConvertedStreamingRequestsAskForUsage(t *testing.T) {
	anthropic := convertRequest(AnthropicRequest{Model: "kimi-k2.6", Stream: true, Messages: []AMessage{{Role: "user", Content: []byte(`hello`)}}})
	if anthropic.StreamOptions == nil || !anthropic.StreamOptions.IncludeUsage {
		t.Fatalf("anthropic conversion should request stream usage: %+v", anthropic.StreamOptions)
	}
	if req, err := responses.Decode([]byte(`{"model":"kimi-k2.6","stream":true,"input":"hello"}`)); err != nil {
		t.Fatal(err)
	} else if chat := req.ToChat(); chat.StreamOptions == nil || !chat.StreamOptions.IncludeUsage {
		t.Fatalf("responses conversion should request stream usage: %+v", chat.StreamOptions)
	}
	if req, err := responses.Decode([]byte(`{"model":"kimi-k2.6","input":"hello"}`)); err != nil {
		t.Fatal(err)
	} else if chat := req.ToChat(); chat.StreamOptions != nil {
		t.Fatalf("non-streaming conversion should not set stream options: %+v", chat.StreamOptions)
	}
}

func TestConvertedRequestsForwardReasoningEffort(t *testing.T) {
	anthropic := convertRequest(AnthropicRequest{Model: "glm-5.1", Reasoning: []byte(`{"effort":"high"}`), Messages: []AMessage{{Role: "user", Content: []byte(`[{"type":"text","text":"hello"}]`)}}})
	if anthropic.ReasoningEffort != "high" {
		t.Fatalf("anthropic reasoning effort = %q, want high", anthropic.ReasoningEffort)
	}
	if effort := downstreamReasoningEffort([]byte(`{"reasoning":{"depth":2}}`)); effort != "medium" {
		t.Fatalf("responses reasoning effort = %q, want medium", effort)
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

func TestWriteAnthropicResponseIncludesUsage(t *testing.T) {
	body := strings.NewReader(`{"choices":[{"message":{"content":"done"}}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16,"prompt_tokens_details":{"cached_tokens":4}}}`)
	w := httptest.NewRecorder()
	writeAnthropicResponse(w, body, "kimi-k2.6")
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	usage, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatalf("missing usage: %+v", out)
	}
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(5) || usage["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("bad anthropic usage: %+v", usage)
	}
}

func TestStreamAnthropicIncludesFinalUsage(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	w := httptest.NewRecorder()
	streamAnthropic(w, body, "kimi-k2.6")
	out := w.Body.String()
	for _, want := range []string{`"input_tokens":7`, `"output_tokens":3`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStreamAnthropicForwardsToolCalls(t *testing.T) {
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
}

func TestSanitizeRawChatToolMessagesPreservesUnknownFields(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"assistant","content":"","name":"assistant-name","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{}"}}],"audio":{"id":"aud"}},{"role":"assistant","content":"done","refusal":"no"}]}`)
	out := sanitizeRawChatToolMessages(body)
	var req map[string]json.RawMessage
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(req["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("expected inserted placeholder, got %d messages: %s", len(messages), out)
	}
	if _, ok := messages[0]["name"]; !ok {
		t.Fatalf("unknown assistant field name was dropped: %s", messages[0])
	}
	if _, ok := messages[0]["audio"]; !ok {
		t.Fatalf("unknown assistant field audio was dropped: %s", messages[0])
	}
	if _, ok := messages[2]["refusal"]; !ok {
		t.Fatalf("unknown later assistant field refusal was dropped: %s", messages[2])
	}
	var placeholder struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(marshalJSON(messages[1]), &placeholder); err != nil {
		t.Fatal(err)
	}
	if placeholder.Role != "tool" || placeholder.ToolCallID != "call_1" || placeholder.Content != unavailableToolResultContent {
		t.Fatalf("bad placeholder: %+v", placeholder)
	}
}

func TestSanitizeRawChatToolMessagesDropsLateToolMessage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},{"role":"assistant","content":"done"},{"role":"tool","tool_call_id":"call_1","content":"late"}]}`)
	out := sanitizeRawChatToolMessages(body)
	var req map[string]json.RawMessage
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	var roles []struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(req["messages"], &roles); err != nil {
		t.Fatal(err)
	}
	if len(roles) != 3 {
		t.Fatalf("late tool should be dropped and placeholder inserted, got %+v", roles)
	}
	if roles[1].Role != "tool" || roles[1].ToolCallID != "call_1" || roles[1].Content != unavailableToolResultContent {
		t.Fatalf("expected placeholder at index 1, got %+v", roles[1])
	}
	if roles[2].Role != "assistant" || roles[2].Content != "done" {
		t.Fatalf("expected assistant after placeholder, got %+v", roles[2])
	}
}
