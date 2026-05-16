package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

const (
	appName          = "ocgo"
	defaultHost      = "127.0.0.1"
	defaultPort      = 3456
	openAIURL        = "https://opencode.ai/zen/go/v1/chat/completions"
	codexProfileName = "ocgo-launch"
)

var version = "dev"

type Config struct {
	APIKey string `json:"api_key"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
}

type AnthropicRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	System      json.RawMessage `json:"system,omitempty"`
	Messages    []AMessage      `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       []ATool         `json:"tools,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
}

type AMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type ATool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type OAIRequest struct {
	Model       string       `json:"model"`
	Messages    []OAIMessage `json:"messages"`
	Stream      bool         `json:"stream,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	TopP        *float64     `json:"top_p,omitempty"`
	Tools       []OAITool    `json:"tools,omitempty"`
}

type ResponsesRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions,omitempty"`
	Stream       bool            `json:"stream,omitempty"`
	MaxTokens    int             `json:"max_output_tokens,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	Tools        []ResponseTool  `json:"tools,omitempty"`
}

type ResponseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type OAIMessage struct {
	Role             string        `json:"role"`
	Content          any           `json:"content,omitempty"`
	ToolCalls        []OAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
}

type OAIContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *OAIImageURL `json:"image_url,omitempty"`
}

type OAIImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type OAITool struct {
	Type     string      `json:"type"`
	Function OAIFunction `json:"function"`
}

type OAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type OAIToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function OAICallFunction `json:"function"`
}

type OAICallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

var reasoningContentCache = struct {
	sync.Mutex
	byCallID map[string]string
}{byCallID: map[string]string{}}

func main() {
	root := &cobra.Command{Use: appName, Short: "Run Claude Code with OpenCode Go", Version: version}
	root.AddCommand(setupCmd(), listCmd(), launchCmd(), serveCmd(), stopCmd(), statusCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func setupCmd() *cobra.Command {
	var key string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Save your OpenCode Go API key",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(key) == "" {
				key = os.Getenv("OCGO_API_KEY")
			}
			if strings.TrimSpace(key) == "" {
				fmt.Print("OpenCode Go API key: ")
				line, err := bufio.NewReader(os.Stdin).ReadString('\n')
				if err != nil && line == "" {
					return err
				}
				key = line
			}
			cfg := Config{APIKey: strings.TrimSpace(key), Host: defaultHost, Port: defaultPort}
			if cfg.APIKey == "" {
				return errors.New("API key cannot be empty")
			}
			return saveConfig(cfg)
		},
	}
	cmd.Flags().StringVar(&key, "api-key", "", "OpenCode Go API key")
	return cmd
}

func listCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Aliases: []string{"ls", "models"}, Short: "List OpenCode Go models", Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("OpenCode Go models:")
		for _, m := range knownModelIDs() {
			fmt.Printf("  %s\n", m)
		}
	}}
}

func knownModelIDs() []string {
	return []string{"glm-5.1", "glm-5", "kimi-k2.6", "kimi-k2.5", "mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-pro", "mimo-v2-omni", "minimax-m2.7", "minimax-m2.5", "deepseek-v4-pro", "deepseek-v4-flash", "qwen3.6-plus", "qwen3.5-plus"}
}

func modelSupportsImages(model string) bool {
	switch model {
	case "kimi-k2.6", "kimi-k2.5", "mimo-v2-omni":
		return true
	default:
		return false
	}
}

func modelInputModalities(model string) []string {
	if modelSupportsImages(model) {
		return []string{"text", "image"}
	}
	return []string{"text"}
}

func launchCmd() *cobra.Command {
	var model string
	var yes bool
	var codexConfigOnly bool
	cmd := &cobra.Command{Use: "launch", Short: "Launch tools through ocgo"}
	claude := &cobra.Command{Use: "claude [-- claude args...]", Short: "Launch Claude Code through OpenCode Go", Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		base := fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
		serverCmd, err := startLaunchServer(base)
		if err != nil {
			return err
		}
		if serverCmd != nil {
			defer stopManagedServer(serverCmd)
		}
		claudeArgs := append([]string{}, args...)
		if yes {
			claudeArgs = append([]string{"--dangerously-skip-permissions"}, claudeArgs...)
		}
		bin, err := exec.LookPath("claude")
		if err != nil {
			return fmt.Errorf("claude not found in PATH: %w", err)
		}
		c := exec.Command(bin, claudeArgs...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		c.Env = append(os.Environ(), "ANTHROPIC_BASE_URL="+base, "ANTHROPIC_AUTH_TOKEN=unused")
		if model != "" {
			c.Env = append(c.Env, "ANTHROPIC_MODEL="+model, "ANTHROPIC_SMALL_FAST_MODEL="+model)
		}
		return c.Run()
	}}
	claude.Flags().StringVar(&model, "model", "", "OpenCode Go model ID")
	claude.Flags().BoolVar(&yes, "yes", false, "Allow Claude Code to skip permission prompts")
	codex := &cobra.Command{Use: "codex [-- codex args...]", Short: "Launch Codex CLI through OpenCode Go", Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		base := fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
		if err := ensureCodexConfig(base); err != nil {
			return fmt.Errorf("failed to configure codex: %w", err)
		}
		if codexConfigOnly {
			fmt.Printf("Configured Codex profile %q in %s\n", codexProfileName, codexConfigFile())
			return nil
		}
		if err := checkCodexVersion(); err != nil {
			return err
		}
		serverCmd, err := startLaunchServer(base)
		if err != nil {
			return err
		}
		if serverCmd != nil {
			defer stopManagedServer(serverCmd)
		}
		codexArgs := []string{"--profile", codexProfileName}
		if model != "" {
			codexArgs = append(codexArgs, "-m", model)
		}
		codexArgs = append(codexArgs, args...)
		bin, err := exec.LookPath("codex")
		if err != nil {
			return fmt.Errorf("codex not found in PATH; install with: npm install -g @openai/codex: %w", err)
		}
		c := exec.Command(bin, codexArgs...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		c.Env = append(os.Environ(), "OPENAI_API_KEY=ocgo")
		return c.Run()
	}}
	codex.Flags().StringVar(&model, "model", "", "OpenCode Go model ID")
	codex.Flags().BoolVar(&codexConfigOnly, "config", false, "Configure Codex profile without launching")
	cmd.AddCommand(claude, codex)
	return cmd
}

func serveCmd() *cobra.Command {
	var background bool
	cmd := &cobra.Command{Use: "serve", Short: "Start local Anthropic-compatible proxy", RunE: func(cmd *cobra.Command, args []string) error {
		if background {
			return startBackground()
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return runServer(cfg)
	}}
	cmd.Flags().BoolVarP(&background, "background", "b", false, "Run proxy in the background")
	return cmd
}

func stopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop", Short: "Stop background proxy", RunE: func(cmd *cobra.Command, args []string) error {
		pid, err := readPID()
		if err != nil {
			cfg, cfgErr := loadConfig()
			if cfgErr != nil {
				return errors.New("proxy is not running")
			}
			pid, err = findListenerPID(cfg.Port)
			if err != nil {
				return errors.New("proxy is not running")
			}
		}
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		_ = os.Remove(pidFile())
		if err := p.Kill(); err != nil {
			return err
		}
		fmt.Printf("Stopped proxy process %d\n", pid)
		return nil
	}}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show proxy status", Run: func(cmd *cobra.Command, args []string) {
		cfg, err := loadConfig()
		if err != nil || !healthy(fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)) {
			fmt.Println("Proxy is not running")
			return
		}
		if pid, err := readPID(); err == nil {
			fmt.Printf("Proxy is running on %s:%d (PID %d)\n", cfg.Host, cfg.Port, pid)
			return
		}
		if pid, err := findListenerPID(cfg.Port); err == nil {
			fmt.Printf("Proxy is running on %s:%d (PID %d, discovered from listener)\n", cfg.Host, cfg.Port, pid)
			return
		}
		fmt.Printf("Proxy is running on %s:%d (no ocgo PID file)\n", cfg.Host, cfg.Port)
	}}
}

func runServer(cfg Config) error {
	if err := os.MkdirAll(configDir(), 0755); err == nil {
		_ = os.WriteFile(pidFile(), []byte(fmt.Sprint(os.Getpid())), 0644)
		defer os.Remove(pidFile())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/v1/messages/count_tokens", countTokens)
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) { proxyMessages(w, r, cfg) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { proxyChatCompletions(w, r, cfg) })
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) { proxyResponses(w, r, cfg) })
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	fmt.Printf("ocgo proxy listening on http://%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

func proxyMessages(w http.ResponseWriter, r *http.Request, cfg Config) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var ar AnthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	or := convertRequest(ar)
	if err := validateImageSupport(or); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body, _ := json.Marshal(or)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, openAIURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if ar.Stream {
		streamAnthropic(w, resp.Body, or.Model)
		return
	}
	writeAnthropicResponse(w, resp.Body, or.Model)
}

func proxyChatCompletions(w http.ResponseWriter, r *http.Request, cfg Config) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	body, err = prepareChatBody(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, openAIURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func proxyResponses(w http.ResponseWriter, r *http.Request, cfg Config) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var rr ResponsesRequest
	if err := json.NewDecoder(r.Body).Decode(&rr); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	or := responsesToChat(rr)
	if err := validateImageSupport(or); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body, _ := json.Marshal(or)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, openAIURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if rr.Stream {
		streamResponses(w, resp.Body, or.Model)
		return
	}
	writeResponsesResponse(w, resp.Body, or.Model)
}

func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func prepareChatBody(body []byte) ([]byte, error) {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body, nil
	}
	model, _ := req["model"].(string)
	if !rawChatBodyHasImages(req) {
		return body, nil
	}
	if !modelSupportsImages(model) {
		return nil, unsupportedImageModelError(model)
	}
	changed := stripRawChatImageDetails(req)
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, nil
	}
	return out, nil
}

func rawChatBodyHasImages(req map[string]any) bool {
	messages, _ := req["messages"].([]any)
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if contentHasImage(msg["content"]) {
			return true
		}
	}
	return false
}

func validateImageSupport(or OAIRequest) error {
	if requestHasImages(or) && !modelSupportsImages(or.Model) {
		return unsupportedImageModelError(or.Model)
	}
	return nil
}

func unsupportedImageModelError(model string) error {
	if model == "" {
		model = "unknown"
	}
	return fmt.Errorf("model %s does not support image inputs", model)
}

func stripRawChatImageDetails(req map[string]any) bool {
	changed := false
	messages, _ := req["messages"].([]any)
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts, _ := msg["content"].([]any)
		for _, part := range parts {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := p["detail"]; ok {
				delete(p, "detail")
				changed = true
			}
			image, ok := p["image_url"].(map[string]any)
			if !ok {
				continue
			}
			if _, ok := image["detail"]; ok {
				delete(image, "detail")
				changed = true
			}
		}
	}
	return changed
}

func convertRequest(ar AnthropicRequest) OAIRequest {
	model := ar.Model
	if model == "" || strings.HasPrefix(model, "claude-") {
		model = "kimi-k2.6"
	}
	out := OAIRequest{Model: model, Stream: ar.Stream, MaxTokens: ar.MaxTokens, Temperature: ar.Temperature, TopP: ar.TopP}
	if sys := systemText(ar.System); sys != "" {
		out.Messages = append(out.Messages, OAIMessage{Role: "system", Content: sys})
	}
	for _, m := range ar.Messages {
		out.Messages = append(out.Messages, contentToOpenAI(m)...)
	}
	for _, t := range ar.Tools {
		out.Tools = append(out.Tools, OAITool{Type: "function", Function: OAIFunction{Name: t.Name, Description: t.Description, Parameters: t.InputSchema}})
	}
	return out
}

func responsesToChat(rr ResponsesRequest) OAIRequest {
	model := rr.Model
	if model == "" {
		model = "kimi-k2.6"
	}
	out := OAIRequest{Model: model, Stream: rr.Stream, MaxTokens: rr.MaxTokens, Temperature: rr.Temperature, TopP: rr.TopP}
	if rr.Instructions != "" {
		out.Messages = append(out.Messages, OAIMessage{Role: "system", Content: rr.Instructions})
	}
	out.Messages = append(out.Messages, responsesInputToMessages(rr.Input)...)
	for _, t := range rr.Tools {
		if t.Type == "function" || t.Name != "" {
			out.Tools = append(out.Tools, OAITool{Type: "function", Function: OAIFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
		}
	}
	return out
}

func requestHasImages(or OAIRequest) bool {
	for _, m := range or.Messages {
		if contentHasImage(m.Content) {
			return true
		}
	}
	return false
}

func contentHasImage(content any) bool {
	switch v := content.(type) {
	case []OAIContentPart:
		for _, part := range v {
			if part.Type == "image_url" && part.ImageURL != nil && part.ImageURL.URL != "" {
				return true
			}
		}
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := m["type"].(string); typ == "image_url" || typ == "input_image" {
				return true
			}
		}
	}
	return false
}

func responsesInputToMessages(raw json.RawMessage) []OAIMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []OAIMessage{{Role: "user", Content: s}}
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return []OAIMessage{{Role: "user", Content: string(raw)}}
	}
	var out []OAIMessage
	var pendingCalls []OAIToolCall
	for _, item := range items {
		var typ, role string
		_ = json.Unmarshal(item["type"], &typ)
		_ = json.Unmarshal(item["role"], &role)
		switch typ {
		case "message", "":
			if role == "developer" {
				role = "system"
			}
			if role == "" {
				role = "user"
			}
			out = append(out, OAIMessage{Role: role, Content: responsesContent(item["content"])})
		case "function_call":
			var id, callID, name, args string
			_ = json.Unmarshal(item["id"], &id)
			_ = json.Unmarshal(item["call_id"], &callID)
			_ = json.Unmarshal(item["name"], &name)
			_ = json.Unmarshal(item["arguments"], &args)
			if callID == "" {
				callID = id
			}
			pendingCalls = append(pendingCalls, OAIToolCall{ID: callID, Type: "function", Function: OAICallFunction{Name: name, Arguments: args}})
		case "function_call_output":
			if len(pendingCalls) > 0 {
				out = append(out, assistantToolCallsMessage(pendingCalls))
				pendingCalls = nil
			}
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			out = append(out, OAIMessage{Role: "tool", ToolCallID: callID, Content: responsesContentText(item["output"])})
		}
	}
	if len(pendingCalls) > 0 {
		out = append(out, assistantToolCallsMessage(pendingCalls))
	}
	return out
}

func assistantToolCallsMessage(calls []OAIToolCall) OAIMessage {
	return OAIMessage{Role: "assistant", ToolCalls: calls, ReasoningContent: cachedReasoningContent(calls)}
}

func cachedReasoningContent(calls []OAIToolCall) string {
	reasoningContentCache.Lock()
	defer reasoningContentCache.Unlock()
	for _, call := range calls {
		if reasoning := reasoningContentCache.byCallID[call.ID]; reasoning != "" {
			return reasoning
		}
	}
	if len(calls) > 0 {
		// Moonshot/Kimi rejects follow-up assistant tool-call messages when
		// thinking is enabled unless reasoning_content is present. Some
		// OpenAI-compatible streams omit reasoning_content on the initial tool
		// call, so provide a minimal placeholder for replayed tool-call history.
		return "Tool call requested."
	}
	return ""
}

func cacheReasoningContent(calls []OAIToolCall, reasoning string) {
	if reasoning == "" || len(calls) == 0 {
		return
	}
	reasoningContentCache.Lock()
	defer reasoningContentCache.Unlock()
	for _, call := range calls {
		if call.ID != "" {
			reasoningContentCache.byCallID[call.ID] = reasoning
		}
	}
}

func responsesContent(raw json.RawMessage) any {
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
	var out []OAIContentPart
	hasImage := false
	for _, p := range parts {
		var typ string
		_ = json.Unmarshal(p["type"], &typ)
		switch typ {
		case "input_text", "output_text", "text":
			for _, key := range []string{"text", "output_text"} {
				var v string
				if json.Unmarshal(p[key], &v) == nil {
					text.WriteString(v)
					out = append(out, OAIContentPart{Type: "text", Text: v})
					break
				}
			}
		case "input_image", "image_url":
			if image := responsesImageURL(p); image != nil {
				hasImage = true
				out = append(out, OAIContentPart{Type: "image_url", ImageURL: image})
			}
		}
	}
	if hasImage {
		return out
	}
	return text.String()
}

func responsesImageURL(p map[string]json.RawMessage) *OAIImageURL {
	var url string
	if json.Unmarshal(p["image_url"], &url) != nil {
		var obj struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(p["image_url"], &obj) == nil {
			url = obj.URL
		}
	}
	if url == "" {
		_ = json.Unmarshal(p["url"], &url)
	}
	if url == "" {
		return nil
	}
	return &OAIImageURL{URL: url}
}

func responsesContentText(raw json.RawMessage) string {
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
	var b strings.Builder
	for _, p := range parts {
		for _, key := range []string{"text", "output_text"} {
			var v string
			if json.Unmarshal(p[key], &v) == nil {
				b.WriteString(v)
			}
		}
	}
	return b.String()
}

func contentToOpenAI(m AMessage) []OAIMessage {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []OAIMessage{{Role: m.Role, Content: s}}
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(m.Content, &blocks) != nil {
		return []OAIMessage{{Role: m.Role, Content: string(m.Content)}}
	}
	var text strings.Builder
	var parts []OAIContentPart
	hasImage := false
	var calls []OAIToolCall
	var toolMsgs []OAIMessage
	for _, b := range blocks {
		var typ string
		_ = json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			var v string
			_ = json.Unmarshal(b["text"], &v)
			text.WriteString(v)
			if v != "" {
				parts = append(parts, OAIContentPart{Type: "text", Text: v})
			}
		case "image":
			if image := anthropicImageURL(b); image != nil {
				hasImage = true
				parts = append(parts, OAIContentPart{Type: "image_url", ImageURL: image})
			}
		case "tool_use":
			var id, name string
			_ = json.Unmarshal(b["id"], &id)
			_ = json.Unmarshal(b["name"], &name)
			args := "{}"
			if raw := b["input"]; len(raw) > 0 {
				args = string(raw)
			}
			calls = append(calls, OAIToolCall{ID: id, Type: "function", Function: OAICallFunction{Name: name, Arguments: args}})
		case "tool_result":
			var id string
			_ = json.Unmarshal(b["tool_use_id"], &id)
			toolMsgs = append(toolMsgs, OAIMessage{Role: "tool", ToolCallID: id, Content: blockText(b["content"])})
		}
	}
	if len(calls) > 0 {
		msg := assistantToolCallsMessage(calls)
		msg.Content = openAIContentValue(text.String(), parts, hasImage)
		return []OAIMessage{msg}
	}
	if len(toolMsgs) > 0 {
		out := append([]OAIMessage{}, toolMsgs...)
		if userText := strings.TrimSpace(text.String()); userText != "" {
			// Anthropic can send a user's next text in the same content array as
			// tool_result blocks. Preserve that text as the next user message;
			// dropping it makes the model answer the previous tool result again.
			out = append(out, OAIMessage{Role: m.Role, Content: userText})
		}
		return out
	}
	return []OAIMessage{{Role: m.Role, Content: openAIContentValue(text.String(), parts, hasImage)}}
}

func openAIContentValue(text string, parts []OAIContentPart, hasImage bool) any {
	if hasImage {
		return parts
	}
	return text
}

func anthropicImageURL(b map[string]json.RawMessage) *OAIImageURL {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(b["source"], &source) != nil {
		return nil
	}
	if source.URL != "" || source.Type == "url" {
		if source.URL == "" {
			return nil
		}
		return &OAIImageURL{URL: source.URL}
	}
	if source.Data == "" {
		return nil
	}
	if strings.HasPrefix(source.Data, "data:") {
		return &OAIImageURL{URL: source.Data}
	}
	mediaType := source.MediaType
	if mediaType == "" {
		mediaType = "image/png"
	}
	return &OAIImageURL{URL: "data:" + mediaType + ";base64," + source.Data}
}

func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return blockText(raw)
}

func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, x := range blocks {
		var t string
		if json.Unmarshal(x["text"], &t) == nil {
			b.WriteString(t)
		}
	}
	return b.String()
}

func streamAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"ocgo\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n", model)
	if flusher != nil {
		flusher.Flush()
	}
	textStarted := false
	textIndex := -1
	nextIndex := 0
	toolIndexes := map[int]int{}
	var tools []streamedResponseToolCall
	var reasoning strings.Builder
	s := bufio.NewScanner(body)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		chunk := parseOpenAIStreamChunk([]byte(data))
		if chunk.ReasoningContent != "" {
			reasoning.WriteString(chunk.ReasoningContent)
		}
		if chunk.Content != "" {
			if !textStarted {
				textStarted = true
				textIndex = nextIndex
				nextIndex++
				fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", textIndex)
			}
			b, _ := json.Marshal(chunk.Content)
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", textIndex, b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		for _, tc := range chunk.ToolCalls {
			toolPos, ok := toolIndexes[tc.Index]
			if !ok {
				callID := tc.ID
				if callID == "" {
					callID = fmt.Sprintf("call_%d", tc.Index)
				}
				toolPos = len(tools)
				toolIndexes[tc.Index] = toolPos
				blockIndex := nextIndex
				nextIndex++
				tools = append(tools, streamedResponseToolCall{OutputIndex: blockIndex, Call: OAIToolCall{ID: callID, Type: "function", Function: OAICallFunction{Name: tc.Name}}})
				idJSON, _ := json.Marshal(callID)
				nameJSON, _ := json.Marshal(tc.Name)
				fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":%s,\"name\":%s,\"input\":{}}}\n\n", blockIndex, idJSON, nameJSON)
			}
			if tc.ID != "" {
				tools[toolPos].Call.ID = tc.ID
			}
			if tc.Name != "" {
				tools[toolPos].Call.Function.Name = tc.Name
			}
			if tc.Arguments != "" {
				tools[toolPos].Call.Function.Arguments += tc.Arguments
				b, _ := json.Marshal(tc.Arguments)
				fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}}\n\n", tools[toolPos].OutputIndex, b)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	var calls []OAIToolCall
	for _, tool := range tools {
		calls = append(calls, tool.Call)
	}
	cacheReasoningContent(calls, reasoning.String())
	if textStarted {
		fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", textIndex)
	}
	for _, tool := range tools {
		fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", tool.OutputIndex)
	}
	stopReason := "end_turn"
	if len(tools) > 0 {
		stopReason = "tool_use"
	}
	fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q,\"stop_sequence\":null},\"usage\":{\"output_tokens\":0}}\n\n", stopReason)
	fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

func openAITextDelta(data []byte) string {
	var v struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(data, &v)
	if len(v.Choices) == 0 {
		return ""
	}
	return v.Choices[0].Delta.Content
}

func writeAnthropicResponse(w http.ResponseWriter, body io.Reader, model string) {
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(body).Decode(&v)
	text := ""
	if len(v.Choices) > 0 {
		text = v.Choices[0].Message.Content
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "ocgo", "type": "message", "role": "assistant", "model": model, "content": []map[string]string{{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}})
}

func streamResponses(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	id := "resp_ocgo"
	writeResponseEvent(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "model": model, "status": "in_progress", "output": []any{}}})
	if flusher != nil {
		flusher.Flush()
	}
	messageStarted := false
	messageDone := false
	messageOutputIndex := -1
	nextOutputIndex := 0
	var text strings.Builder
	var reasoning strings.Builder
	toolIndexes := map[int]int{}
	var tools []streamedResponseToolCall
	s := bufio.NewScanner(body)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		chunk := parseOpenAIStreamChunk([]byte(data))
		if chunk.ReasoningContent != "" {
			reasoning.WriteString(chunk.ReasoningContent)
		}
		if chunk.Content != "" {
			if !messageStarted {
				messageStarted = true
				messageOutputIndex = nextOutputIndex
				nextOutputIndex++
				writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_ocgo", "type": "message", "role": "assistant", "content": []any{}}})
				writeResponseEvent(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_ocgo", "output_index": messageOutputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
			}
			text.WriteString(chunk.Content)
			writeResponseEvent(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_ocgo", "output_index": messageOutputIndex, "content_index": 0, "delta": chunk.Content})
			if flusher != nil {
				flusher.Flush()
			}
		}
		for _, tc := range chunk.ToolCalls {
			toolPos, ok := toolIndexes[tc.Index]
			if !ok {
				callID := tc.ID
				if callID == "" {
					callID = fmt.Sprintf("call_%d", tc.Index)
				}
				toolPos = len(tools)
				toolIndexes[tc.Index] = toolPos
				outputIndex := nextOutputIndex
				nextOutputIndex++
				tools = append(tools, streamedResponseToolCall{OutputIndex: outputIndex, Call: OAIToolCall{ID: callID, Type: "function", Function: OAICallFunction{Name: tc.Name}}})
				writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": map[string]any{"id": callID, "type": "function_call", "call_id": callID, "name": tc.Name, "arguments": ""}})
			}
			if tc.ID != "" {
				tools[toolPos].Call.ID = tc.ID
			}
			if tc.Name != "" {
				tools[toolPos].Call.Function.Name = tc.Name
			}
			if tc.Arguments != "" {
				tools[toolPos].Call.Function.Arguments += tc.Arguments
				writeResponseEvent(w, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": tools[toolPos].Call.ID, "output_index": tools[toolPos].OutputIndex, "delta": tc.Arguments})
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	var toolCalls []OAIToolCall
	for _, tool := range tools {
		toolCalls = append(toolCalls, tool.Call)
	}
	cacheReasoningContent(toolCalls, reasoning.String())
	if messageStarted && !messageDone {
		messageDone = true
		writeResponseEvent(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_ocgo", "output_index": messageOutputIndex, "content_index": 0, "text": text.String()})
		writeResponseEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_ocgo", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text.String()}}}})
	}
	var output []any
	if messageStarted {
		output = append(output, map[string]any{"id": "msg_ocgo", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text.String()}}})
	}
	for _, tool := range tools {
		call := tool.Call
		item := map[string]any{"id": call.ID, "type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments}
		writeResponseEvent(w, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": call.ID, "output_index": tool.OutputIndex, "arguments": call.Function.Arguments})
		writeResponseEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tool.OutputIndex, "item": item})
		output = append(output, item)
	}
	writeResponseEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "model": model, "status": "completed", "output": output}})
}

type streamedResponseToolCall struct {
	OutputIndex int
	Call        OAIToolCall
}

type openAIStreamToolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

type openAIStreamChunk struct {
	Content          string
	ReasoningContent string
	ToolCalls        []openAIStreamToolCall
}

func parseOpenAIStreamChunk(data []byte) openAIStreamChunk {
	var v struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(data, &v)
	if len(v.Choices) == 0 {
		return openAIStreamChunk{}
	}
	delta := v.Choices[0].Delta
	out := openAIStreamChunk{Content: delta.Content, ReasoningContent: delta.ReasoningContent}
	for _, tc := range delta.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, openAIStreamToolCall{Index: tc.Index, ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out
}

func writeResponseEvent(w io.Writer, event string, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func writeResponsesResponse(w http.ResponseWriter, body io.Reader, model string) {
	var v struct {
		Choices []struct {
			Message struct {
				Content          string        `json:"content"`
				ReasoningContent string        `json:"reasoning_content"`
				ToolCalls        []OAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(body).Decode(&v)
	text := ""
	var output []any
	if len(v.Choices) > 0 {
		text = v.Choices[0].Message.Content
		if len(v.Choices[0].Message.ToolCalls) > 0 {
			cacheReasoningContent(v.Choices[0].Message.ToolCalls, v.Choices[0].Message.ReasoningContent)
			for _, call := range v.Choices[0].Message.ToolCalls {
				output = append(output, map[string]any{"id": call.ID, "type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
			}
		}
	}
	if len(output) == 0 {
		output = append(output, map[string]any{"id": "msg_ocgo", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text}}})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_ocgo", "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "completed", "output": output, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}})
}

func countTokens(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": 0})
}

func ensureServer(base string) error {
	if healthy(base) {
		return nil
	}
	if err := startBackground(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if healthy(base) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("proxy did not start")
}

func startLaunchServer(base string) (*exec.Cmd, error) {
	if healthy(base) {
		return nil, nil
	}
	cmd, err := startServerProcess(false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if healthy(base) {
			return cmd, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	stopManagedServer(cmd)
	return nil, errors.New("proxy did not start")
}

func stopManagedServer(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	_ = os.Remove(pidFile())
}

func healthy(base string) bool {
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(base + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func startBackground() error {
	_, err := startServerProcess(true)
	return err
}

func startServerProcess(detached bool) (*exec.Cmd, error) {
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(configDir(), 0755); err != nil {
		return nil, err
	}
	args := []string{"serve"}
	cmd := exec.Command(bin, args...)
	logf, err := os.OpenFile(filepath.Join(configDir(), "ocgo.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Stdin = nil
	if detached && runtime.GOOS != "windows" {
		cmd.SysProcAttr = detachedAttrs()
	}
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return nil, err
	}
	return cmd, nil
}

func configDir() string  { home, _ := os.UserHomeDir(); return filepath.Join(home, ".config", "ocgo") }
func configFile() string { return filepath.Join(configDir(), "config.json") }
func pidFile() string    { return filepath.Join(configDir(), "ocgo.pid") }

func codexConfigFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

func codexModelCatalogFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "ocgo-models.json")
}

func ensureCodexConfig(base string) error {
	path := codexConfigFile()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := writeCodexModelCatalog(codexModelCatalogFile()); err != nil {
		return err
	}
	return writeCodexProfile(path, strings.TrimRight(base, "/")+"/v1/")
}

func writeCodexProfile(path, baseURL string) error {
	catalogPath := codexModelCatalogFile()
	sections := []struct {
		header string
		lines  []string
	}{
		{fmt.Sprintf("[profiles.%s]", codexProfileName), []string{fmt.Sprintf("openai_base_url = %q", baseURL), `forced_login_method = "api"`, fmt.Sprintf("model_provider = %q", codexProfileName), fmt.Sprintf("model_catalog_json = %q", catalogPath), `model_reasoning_effort = "minimal"`, `model_reasoning_summary = "none"`}},
		{fmt.Sprintf("[model_providers.%s]", codexProfileName), []string{`name = "OpenCode Go"`, fmt.Sprintf("base_url = %q", baseURL), `wire_api = "responses"`}},
	}
	b, err := os.ReadFile(path)
	text := ""
	if err == nil {
		text = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, s := range sections {
		block := strings.Join(append([]string{s.header}, s.lines...), "\n") + "\n"
		if idx := strings.Index(text, s.header); idx >= 0 {
			rest := text[idx+len(s.header):]
			if endIdx := strings.Index(rest, "\n["); endIdx >= 0 {
				text = text[:idx] + block + rest[endIdx+1:]
			} else {
				text = text[:idx] + block
			}
		} else {
			if text != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			if text != "" {
				text += "\n"
			}
			text += block
		}
	}
	return os.WriteFile(path, []byte(text), 0644)
}

func writeCodexModelCatalog(path string) error {
	models := make([]map[string]any, 0, len(knownModelIDs()))
	for i, id := range knownModelIDs() {
		models = append(models, map[string]any{
			"slug":                             id,
			"display_name":                     id,
			"description":                      "OpenCode Go model",
			"default_reasoning_level":          nil,
			"supported_reasoning_levels":       []any{},
			"shell_type":                       "shell_command",
			"visibility":                       "list",
			"supported_in_api":                 true,
			"priority":                         i,
			"availability_nux":                 nil,
			"upgrade":                          nil,
			"base_instructions":                "You are Codex, a coding agent running in a terminal-based coding assistant.",
			"supports_reasoning_summaries":     false,
			"default_reasoning_summary":        "none",
			"support_verbosity":                false,
			"default_verbosity":                nil,
			"apply_patch_tool_type":            nil,
			"web_search_tool_type":             "text",
			"truncation_policy":                map[string]any{"mode": "tokens", "limit": 10000},
			"supports_parallel_tool_calls":     false,
			"supports_image_detail_original":   false,
			"context_window":                   128000,
			"max_context_window":               128000,
			"auto_compact_token_limit":         nil,
			"effective_context_window_percent": 95,
			"experimental_supported_tools":     []any{},
			"input_modalities":                 modelInputModalities(id),
			"supports_search_tool":             false,
		})
	}
	b, err := json.MarshalIndent(map[string]any{"models": models}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0644)
}

func checkCodexVersion() error {
	if _, err := exec.LookPath("codex"); err != nil {
		return fmt.Errorf("codex is not installed, install with: npm install -g @openai/codex")
	}
	out, err := exec.Command("codex", "--version").Output()
	if err != nil {
		return fmt.Errorf("failed to get codex version: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return fmt.Errorf("unexpected codex version output: %s", string(out))
	}
	version := fields[len(fields)-1]
	if compareVersions(version, "0.81.0") < 0 {
		return fmt.Errorf("codex version %s is too old, minimum required is 0.81.0; update with: npm update -g @openai/codex", version)
	}
	return nil
}

func compareVersions(a, b string) int {
	ap, bp := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if ap[i] > bp[i] {
			return 1
		}
		if ap[i] < bp[i] {
			return -1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	fields := strings.Split(v, ".")
	var out [3]int
	for i := 0; i < len(fields) && i < 3; i++ {
		part := fields[i]
		for j, r := range part {
			if r < '0' || r > '9' {
				part = part[:j]
				break
			}
		}
		out[i], _ = strconv.Atoi(part)
	}
	return out
}

func saveConfig(cfg Config) error {
	if err := os.MkdirAll(configDir(), 0755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configFile(), append(b, '\n'), 0600); err != nil {
		return err
	}
	fmt.Printf("Saved config to %s\n", configFile())
	return nil
}

func loadConfig() (Config, error) {
	cfg := Config{Host: defaultHost, Port: defaultPort, APIKey: os.Getenv("OCGO_API_KEY")}
	b, err := os.ReadFile(configFile())
	if err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	if cfg.APIKey == "" {
		return cfg, errors.New("missing API key; run: ocgo setup")
	}
	if cfg.Host == "" {
		cfg.Host = defaultHost
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	return cfg, nil
}

func readPID() (int, error) {
	b, err := os.ReadFile(pidFile())
	if err != nil {
		return 0, err
	}
	var pid int
	_, err = fmt.Sscan(string(b), &pid)
	return pid, err
}
