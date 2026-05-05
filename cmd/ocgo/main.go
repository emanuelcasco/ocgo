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
	"time"

	"github.com/spf13/cobra"
)

const (
	appName     = "ocgo"
	defaultHost = "127.0.0.1"
	defaultPort = 3456
	openAIURL   = "https://opencode.ai/zen/go/v1/chat/completions"
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

type OAIMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []OAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
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
		for _, m := range []string{"glm-5.1", "glm-5", "kimi-k2.6", "kimi-k2.5", "mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-pro", "mimo-v2-omni", "minimax-m2.7", "minimax-m2.5", "deepseek-v4-pro", "deepseek-v4-flash", "qwen3.6-plus", "qwen3.5-plus"} {
			fmt.Printf("  %s\n", m)
		}
	}}
}

func launchCmd() *cobra.Command {
	var model string
	var yes bool
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
	cmd.AddCommand(claude)
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
		return []OAIMessage{{Role: "assistant", Content: text.String(), ToolCalls: calls}}
	}
	if len(toolMsgs) > 0 {
		return toolMsgs
	}
	return []OAIMessage{{Role: m.Role, Content: text.String()}}
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
	fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	if flusher != nil {
		flusher.Flush()
	}
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
		if delta := openAITextDelta([]byte(data)); delta != "" {
			b, _ := json.Marshal(delta)
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":0}}\n\n")
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

func findListenerPID(port int) (int, error) {
	if port == 0 {
		return 0, errors.New("missing port")
	}
	out, err := exec.Command("lsof", "-nP", "-tiTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err == nil && pid > 0 {
			return pid, nil
		}
	}
	return 0, errors.New("no listener found")
}
