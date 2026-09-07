package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

const (
	appName                            = "ocgo"
	defaultHost                        = "127.0.0.1"
	defaultPort                        = 3456
	openAIURL                          = "https://opencode.ai/zen/go/v1/chat/completions"
	codexProfileName                   = "ocgo-launch"
	maxAnthropicToolResultContentChars = 120000
)

var version = "dev"

var anthropicURL = "https://opencode.ai/zen/go/v1/messages"

const (
	remoteModelsURL   = "https://models.dev/api.json"
	officialModelsURL = "https://opencode.ai/zen/go/v1/models"
)

// openCodeSessionHeader is required by OpenCode Go on every model request so it
// can pin a conversation to one backend and keep prompt caches warm. See
// https://opencode.ai/docs/go/#where-can-i-use-it.
const openCodeSessionHeader = "X-OpenCode-Session"

// openCodeAffinityHeaders are inbound headers ocgo accepts as a client-supplied
// session id. They let a caller keep one scope across a shared proxy; when none
// is present ocgo falls back to a per-process id.
var openCodeAffinityHeaders = []string{
	openCodeSessionHeader,
	"X-Session-Id",
	"X-Session-Affinity",
	"Conversation-Id",
	"X-Conversation-Id",
}

// proxySessionID is the opaque id ocgo presents to OpenCode Go. The launched
// tools (Claude Code, Codex) do not send one of their own, so ocgo mints a
// single random id for the lifetime of the proxy process; each `ocgo launch`
// owns its proxy, which scopes the id to one conversation in practice.
var proxySessionID = newProxySessionID()

func newProxySessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		b[6] = (b[6] & 0x0f) | 0x40 // version 4
		b[8] = (b[8] & 0x3f) | 0x80 // variant 10
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return fmt.Sprintf("ocgo-%d-%d", time.Now().UnixNano(), os.Getpid())
}

// applyOpenCodeSessionHeaders stamps an x-opencode-session header onto an
// outbound request. A non-empty client-supplied affinity header wins so a
// conversation keeps one id even across proxy restarts; otherwise the
// per-process id is used. The header is never left empty.
func applyOpenCodeSessionHeaders(dst http.Header, src http.Header) {
	for _, name := range openCodeAffinityHeaders {
		if v := strings.TrimSpace(src.Get(name)); v != "" {
			dst.Set(openCodeSessionHeader, v)
			return
		}
	}
	if strings.TrimSpace(dst.Get(openCodeSessionHeader)) == "" {
		dst.Set(openCodeSessionHeader, proxySessionID)
	}
}

// officialModelsResponse matches the OpenCode Go /v1/models response shape.
type officialModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// remoteModelInfo represents the subset of models.dev metadata ocgo needs.
type remoteModelInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

// remoteAPIResponse is the top-level structure of the models.dev API response.
type remoteAPIResponse struct {
	OpenCodeGo struct {
		Models map[string]remoteModelInfo `json:"models"`
	} `json:"opencode-go"`
}

// httpClient is shared by short-lived metadata fetches. Long-running model
// inference requests keep their larger per-request timeouts below.
var httpClient = &http.Client{Timeout: 8 * time.Second}

type lazyFetcher[T any] struct {
	mu      sync.RWMutex
	data    T
	err     error
	fetched bool
	fetch   func() (T, error)
}

func newLazyFetcher[T any](fetch func() (T, error)) *lazyFetcher[T] {
	return &lazyFetcher[T]{fetch: fetch}
}

func (f *lazyFetcher[T]) get() (T, error) {
	f.mu.RLock()
	if f.fetched {
		data, err := f.data, f.err
		f.mu.RUnlock()
		return data, err
	}
	f.mu.RUnlock()

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetched {
		return f.data, f.err
	}
	f.data, f.err = f.fetch()
	f.fetched = true
	return f.data, f.err
}

func (f *lazyFetcher[T]) refresh() {
	f.mu.Lock()
	var zero T
	f.data = zero
	f.err = nil
	f.fetched = false
	f.mu.Unlock()
	_, _ = f.get()
}

var (
	remoteModels   = newLazyFetcher(fetchRemoteModels)
	officialModels = newLazyFetcher(fetchOfficialModels)
)

type Config struct {
	APIKey string `json:"api_key"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
}

type AnthropicRequest struct {
	Model           string          `json:"model"`
	MaxTokens       int             `json:"max_tokens"`
	System          json.RawMessage `json:"system,omitempty"`
	Messages        []AMessage      `json:"messages"`
	Stream          bool            `json:"stream,omitempty"`
	Tools           []ATool         `json:"tools,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Thinking        json.RawMessage `json:"thinking,omitempty"`
	Reasoning       json.RawMessage `json:"reasoning,omitempty"`
	ReasoningEffort json.RawMessage `json:"reasoning_effort,omitempty"`
	Effort          json.RawMessage `json:"effort,omitempty"`
	Level           json.RawMessage `json:"level,omitempty"`
	Depth           json.RawMessage `json:"depth,omitempty"`
	OutputConfig    json.RawMessage `json:"output_config,omitempty"`
}

type AMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type ATool struct {
	Type           string          `json:"type,omitempty"`
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	InputSchema    json.RawMessage `json:"input_schema,omitempty"`
	MaxUses        int             `json:"max_uses,omitempty"`
	AllowedDomains []string        `json:"allowed_domains,omitempty"`
	BlockedDomains []string        `json:"blocked_domains,omitempty"`
	UserLocation   json.RawMessage `json:"user_location,omitempty"`
}

type OAIRequest struct {
	Model           string            `json:"model"`
	Messages        []OAIMessage      `json:"messages"`
	Stream          bool              `json:"stream,omitempty"`
	StreamOptions   *OAIStreamOptions `json:"stream_options,omitempty"`
	MaxTokens       int               `json:"max_tokens,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	TopP            *float64          `json:"top_p,omitempty"`
	Tools           []OAITool         `json:"tools,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	AnthropicTools  []ATool           `json:"-"`
}

type OAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	MaxTokens       int             `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Tools           []ResponseTool  `json:"tools,omitempty"`
	Thinking        json.RawMessage `json:"thinking,omitempty"`
	Reasoning       json.RawMessage `json:"reasoning,omitempty"`
	ReasoningEffort json.RawMessage `json:"reasoning_effort,omitempty"`
	Effort          json.RawMessage `json:"effort,omitempty"`
	Level           json.RawMessage `json:"level,omitempty"`
	Depth           json.RawMessage `json:"depth,omitempty"`
	OutputConfig    json.RawMessage `json:"output_config,omitempty"`
}

type ResponseTool struct {
	Type              string          `json:"type"`
	Name              string          `json:"name,omitempty"`
	Description       string          `json:"description,omitempty"`
	Parameters        json.RawMessage `json:"parameters,omitempty"`
	SearchContextSize string          `json:"search_context_size,omitempty"`
	UserLocation      json.RawMessage `json:"user_location,omitempty"`
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
	root.AddCommand(setupCmd(), listCmd(), mappingCmd(), launchCmd(), serveCmd(), stopCmd(), statusCmd())
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
		refreshAllModels()
		fmt.Println("OpenCode Go models:")
		for _, m := range knownModelIDs() {
			fmt.Printf("  %s\n", m)
		}
	}}
}

var fallbackModelIDs = []string{"glm-5.1", "glm-5", "kimi-k2.6", "kimi-k2.5", "mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-pro", "mimo-v2-omni", "minimax-m3", "minimax-m2.7", "minimax-m2.5", "deepseek-v4-pro", "deepseek-v4-flash", "qwen3.7-max", "qwen3.6-plus", "qwen3.5-plus"}

func knownModelIDs() []string {
	if ids, err := getOfficialModels(); err == nil && len(ids) > 0 {
		out := append([]string(nil), ids...)
		sort.Strings(out)
		return out
	}
	if models, err := getRemoteModels(); err == nil && len(models) > 0 {
		out := make([]string, 0, len(models))
		for id := range models {
			if strings.TrimSpace(id) != "" {
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return out
	}
	return append([]string(nil), fallbackModelIDs...)
}

type openCodeModelMetadata struct {
	DisplayName             string
	Description             string
	InputModalities         []string
	CodexInputModalities    []string
	ContextWindow           int
	MaxContextWindow        int
	UsesAnthropicEndpoint   bool
	ParallelToolCalls       bool
	SupportsImageOriginal   bool
	SupportsSearchTool      bool
	SupportedReasoning      []any
	DefaultReasoningLevel   any
	ReasoningSummaries      bool
	DefaultReasoningSummary string
}

func modelMetadata(model string) openCodeModelMetadata {
	id := modelID(model)
	meta := openCodeModelMetadata{
		DisplayName:             id,
		Description:             "OpenCode Go model",
		InputModalities:         []string{"text"},
		CodexInputModalities:    []string{"text"},
		ContextWindow:           128000,
		MaxContextWindow:        128000,
		DefaultReasoningLevel:   nil,
		SupportedReasoning:      []any{},
		DefaultReasoningSummary: "none",
	}
	if remote, err := getRemoteModels(); err == nil {
		if rm, ok := remote[id]; ok {
			if strings.TrimSpace(rm.Name) != "" {
				meta.DisplayName = rm.Name
				meta.Description = rm.Name + " via OpenCode Go"
			}
			if len(rm.Modalities.Input) > 0 {
				meta.InputModalities = append([]string(nil), rm.Modalities.Input...)
				meta.CodexInputModalities = codexSupportedModalities(rm.Modalities.Input)
			}
			if rm.Limit.Context > 0 {
				meta.ContextWindow = rm.Limit.Context
				meta.MaxContextWindow = rm.Limit.Context
			}
		}
	}
	switch id {
	case "minimax-m3":
		if meta.DisplayName == id {
			meta.DisplayName = "MiniMax M3"
			meta.Description = "MiniMax M3 via OpenCode Go"
		}
		if meta.ContextWindow == 128000 {
			meta.ContextWindow = 512000
			meta.MaxContextWindow = 512000
		}
		if len(meta.InputModalities) == 1 && meta.InputModalities[0] == "text" {
			meta.InputModalities = []string{"text", "image", "video"}
			meta.CodexInputModalities = []string{"text", "image"}
		}
		meta.UsesAnthropicEndpoint = true
		meta.ParallelToolCalls = true
	case "minimax-m2.7", "minimax-m2.5":
		meta.UsesAnthropicEndpoint = true
	case "qwen3.7-max":
		meta.UsesAnthropicEndpoint = true
		meta.SupportsSearchTool = true
	case "kimi-k2.6", "kimi-k2.5", "mimo-v2-omni":
		if len(meta.InputModalities) == 1 && meta.InputModalities[0] == "text" {
			meta.InputModalities = []string{"text", "image"}
			meta.CodexInputModalities = []string{"text", "image"}
		}
	}
	return meta
}

func codexSupportedModalities(modalities []string) []string {
	out := make([]string, 0, len(modalities))
	seen := map[string]bool{}
	for _, modality := range modalities {
		switch modality {
		case "text", "image":
			if !seen[modality] {
				out = append(out, modality)
				seen[modality] = true
			}
		}
	}
	if len(out) == 0 {
		return []string{"text"}
	}
	return out
}

func fetchRemoteModels() (map[string]remoteModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote models API returned status %d", resp.StatusCode)
	}
	var apiResp remoteAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode remote models: %w", err)
	}
	if apiResp.OpenCodeGo.Models == nil {
		return nil, errors.New("remote models API returned no opencode-go models")
	}
	return apiResp.OpenCodeGo.Models, nil
}

func getRemoteModels() (map[string]remoteModelInfo, error) {
	return remoteModels.get()
}

func fetchOfficialModels() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, officialModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official models API returned status %d", resp.StatusCode)
	}
	var apiResp officialModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode official models: %w", err)
	}
	ids := make([]string, 0, len(apiResp.Data))
	seen := map[string]bool{}
	for _, m := range apiResp.Data {
		id := strings.TrimSpace(m.ID)
		if id != "" && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}

func getOfficialModels() ([]string, error) {
	return officialModels.get()
}

func refreshAllModels() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); officialModels.refresh() }()
	go func() { defer wg.Done(); remoteModels.refresh() }()
	wg.Wait()
}

func defaultModelMappings() map[string]map[string]string {
	return map[string]map[string]string{
		"claude": {},
		"codex":  {},
	}
}

func mappingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mapping", Short: "Manage tool model mappings to OpenCode Go models"}
	cmd.AddCommand(toolMappingCmd("claude"), toolMappingCmd("codex"))
	return cmd
}

func toolMappingCmd(tool string) *cobra.Command {
	cmd := &cobra.Command{Use: tool, Short: fmt.Sprintf("Manage %s model mappings", tool)}
	cmd.AddCommand(&cobra.Command{Use: "show", Short: "Show current mapping", RunE: func(cmd *cobra.Command, args []string) error {
		m, err := loadModelMappings()
		if err != nil {
			return err
		}
		printToolMapping(tool, m[tool])
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "get <source-model>", Short: "Get one mapped OpenCode Go model", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		m, err := loadModelMappings()
		if err != nil {
			return err
		}
		source := strings.TrimSpace(args[0])
		normalized := modelID(source)
		if target := resolveMappedModel(tool, source, m); target != normalized {
			fmt.Printf("%s -> %s\n", source, target)
			return nil
		}
		return fmt.Errorf("no mapping for %q in %s", source, tool)
	}})
	cmd.AddCommand(&cobra.Command{Use: "set <source-model> <opencode-model>", Short: "Set one model mapping", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		source := strings.TrimSpace(args[0])
		target := strings.TrimSpace(args[1])
		if source == "" || target == "" {
			return errors.New("source and target models cannot be empty")
		}
		if !knownOpenCodeModel(target) {
			return fmt.Errorf("unknown OpenCode Go model %q; run `ocgo models`", target)
		}
		m, err := loadModelMappings()
		if err != nil {
			return err
		}
		if m[tool] == nil {
			m[tool] = map[string]string{}
		}
		m[tool][source] = modelID(target)
		if err := saveModelMappings(m); err != nil {
			return err
		}
		fmt.Printf("%s %s -> %s\n", tool, source, modelID(target))
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "unset <source-model>", Aliases: []string{"rm", "remove", "delete"}, Short: "Remove one model mapping", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		source := strings.TrimSpace(args[0])
		if source == "" {
			return errors.New("source model cannot be empty")
		}
		m, err := loadModelMappings()
		if err != nil {
			return err
		}
		if m[tool] == nil {
			m[tool] = map[string]string{}
		}
		if _, ok := m[tool][source]; !ok {
			return fmt.Errorf("no mapping for %q in %s", source, tool)
		}
		delete(m[tool], source)
		if err := saveModelMappings(m); err != nil {
			return err
		}
		fmt.Printf("removed %s mapping for %s\n", tool, source)
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "open", Short: "Open mapping file in $EDITOR", RunE: func(cmd *cobra.Command, args []string) error {
		m, err := loadModelMappings()
		if err != nil {
			return err
		}
		if _, err := os.Stat(modelMappingFile()); errors.Is(err, os.ErrNotExist) {
			if err := saveModelMappings(m); err != nil {
				return err
			}
		}
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		c := exec.Command(editor, modelMappingFile())
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}})
	return cmd
}

func printToolMapping(tool string, mapping map[string]string) {
	fmt.Printf("%s -> OpenCode Go mapping (%s):\n", displayToolName(tool), modelMappingFile())
	if len(mapping) == 0 {
		fmt.Println("  (empty)")
		return
	}
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-24s -> %s\n", k, mapping[k])
	}
}

func displayToolName(tool string) string {
	if tool == "" {
		return tool
	}
	return strings.ToUpper(tool[:1]) + tool[1:]
}

func printLaunchMapping(tool string, mapping map[string]string) {
	if len(mapping) == 0 {
		fmt.Fprintf(os.Stderr, "No OCGO model mappings configured for %s (%s)\n", tool, modelMappingFile())
		return
	}
	fmt.Fprintf(os.Stderr, "OCGO model mapping enabled for %s (%s)\n", tool, modelMappingFile())
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(os.Stderr, "  %s -> %s\n", k, mapping[k])
	}
}

func knownOpenCodeModel(model string) bool {
	model = modelID(model)
	for _, id := range knownModelIDs() {
		if id == model {
			return true
		}
	}
	return false
}

func loadModelMappings() (map[string]map[string]string, error) {
	mappings := defaultModelMappings()
	b, err := os.ReadFile(modelMappingFile())
	if errors.Is(err, os.ErrNotExist) {
		return mappings, nil
	}
	if err != nil {
		return nil, err
	}
	var custom map[string]map[string]string
	if err := json.Unmarshal(b, &custom); err != nil {
		return mappings, nil
	}
	for tool, entries := range custom {
		if mappings[tool] == nil {
			mappings[tool] = map[string]string{}
		}
		for source, target := range entries {
			if strings.TrimSpace(source) != "" && strings.TrimSpace(target) != "" {
				mappings[tool][strings.TrimSpace(source)] = modelID(target)
			}
		}
	}
	return mappings, nil
}

func saveModelMappings(mappings map[string]map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(modelMappingFile()), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(mappings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(modelMappingFile(), append(b, '\n'), 0644)
}

func resolveMappedModel(tool, source string, mappings map[string]map[string]string) string {
	source = strings.TrimSpace(modelID(source))
	entries := mappings[tool]
	if target := entries[source]; target != "" {
		return target
	}
	if tool == "claude" {
		for _, family := range []string{"opus", "sonnet", "haiku"} {
			if source == family || strings.Contains(source, "claude-"+family) {
				if target := entries["claude-"+family]; target != "" {
					return target
				}
			}
		}
	}
	return source
}

func modelID(model string) string {
	return strings.TrimPrefix(strings.TrimSpace(model), "opencode-go/")
}

func modelUsesAnthropicEndpoint(model string) bool {
	return modelMetadata(model).UsesAnthropicEndpoint
}

func modelSupportsImages(model string) bool {
	for _, modality := range modelMetadata(model).InputModalities {
		if modality == "image" {
			return true
		}
	}
	return false
}

func modelInputModalities(model string) []string {
	modalities := modelMetadata(model).InputModalities
	return append([]string(nil), modalities...)
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
		mappings, err := loadModelMappings()
		if err != nil {
			return err
		}
		if model != "" {
			c.Env = append(c.Env,
				"ANTHROPIC_MODEL="+model,
				"ANTHROPIC_SMALL_FAST_MODEL="+model,
				"ANTHROPIC_CUSTOM_MODEL_OPTION="+model,
				"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME="+model+" via OCGO",
				"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION=OpenCode Go model routed through OCGO",
			)
		} else {
			opus := resolveMappedModel("claude", "claude-opus", mappings)
			sonnet := resolveMappedModel("claude", "claude-sonnet", mappings)
			haiku := resolveMappedModel("claude", "claude-haiku", mappings)
			if opus != "claude-opus" {
				c.Env = append(c.Env,
					"ANTHROPIC_DEFAULT_OPUS_MODEL="+opus,
					"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME="+opus+" via OCGO",
					"ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION=OpenCode Go model routed through OCGO",
				)
			}
			if sonnet != "claude-sonnet" {
				c.Env = append(c.Env,
					"ANTHROPIC_DEFAULT_SONNET_MODEL="+sonnet,
					"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME="+sonnet+" via OCGO",
					"ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION=OpenCode Go model routed through OCGO",
				)
			}
			if haiku != "claude-haiku" {
				c.Env = append(c.Env,
					"ANTHROPIC_DEFAULT_HAIKU_MODEL="+haiku,
					"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME="+haiku+" via OCGO",
					"ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION=OpenCode Go model routed through OCGO",
					"ANTHROPIC_SMALL_FAST_MODEL="+haiku,
				)
			}
		}
		printLaunchMapping("claude", mappings["claude"])
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
			fmt.Printf("Configured Codex profile %q in %s\n", codexProfileName, codexProfileConfigFile())
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
		if mappings, err := loadModelMappings(); err == nil {
			printLaunchMapping("codex", mappings["codex"])
		}
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
	if modelUsesAnthropicEndpoint(ar.Model) {
		ar.Model = modelID(ar.Model)
		ensureAnthropicRequestDefaults(&ar)
		resp, err := forwardAnthropic(r.Context(), cfg, ar, r.Header)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
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
	applyOpenCodeSessionHeaders(req.Header, r.Header)
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
	var or OAIRequest
	if json.Unmarshal(body, &or) == nil && modelUsesAnthropicEndpoint(or.Model) {
		or.Model = modelID(or.Model)
		if err := validateImageSupport(or); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := forwardAnthropic(r.Context(), cfg, chatToAnthropic(or), r.Header)
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
		if or.Stream {
			streamChatCompletionsFromAnthropic(w, resp.Body, or.Model)
			return
		}
		writeChatCompletionsResponseFromAnthropic(w, resp.Body, or.Model)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, openAIURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	applyOpenCodeSessionHeaders(req.Header, r.Header)
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
	if modelUsesAnthropicEndpoint(or.Model) {
		or.Model = modelID(or.Model)
		resp, err := forwardAnthropic(r.Context(), cfg, chatToAnthropic(or), r.Header)
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
			streamResponsesFromAnthropic(w, resp.Body, or.Model)
			return
		}
		writeResponsesResponseFromAnthropic(w, resp.Body, or.Model)
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
	applyOpenCodeSessionHeaders(req.Header, r.Header)
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

func forwardAnthropic(ctx context.Context, cfg Config, ar AnthropicRequest, inbound http.Header) (*http.Response, error) {
	normalizeAnthropicRequestForUpstream(&ar)
	body, err := json.Marshal(ar)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", cfg.APIKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	applyOpenCodeSessionHeaders(req.Header, inbound)
	return (&http.Client{Timeout: 10 * time.Minute}).Do(req)
}

func normalizeAnthropicRequestForUpstream(ar *AnthropicRequest) {
	ensureAnthropicRequestDefaults(ar)
	// OpenCode Go's Anthropic-compatible endpoint is stricter than Anthropic's
	// Claude endpoint for some model families (notably qwen3.7-max). Claude Code
	// can send Anthropic-specific prompt-caching and extended-thinking fields that
	// make those upstreams return "Request body format invalid". Keep the core
	// Messages API shape and strip the extensions before forwarding.
	ar.Thinking = nil
	ar.Reasoning = nil
	ar.ReasoningEffort = nil
	ar.Effort = nil
	ar.Level = nil
	ar.Depth = nil
	ar.OutputConfig = nil
	ar.System = normalizeAnthropicSystem(ar.System)
	for i := range ar.Messages {
		ar.Messages[i].Content = normalizeAnthropicContent(ar.Messages[i].Content)
	}
}

func normalizeAnthropicSystem(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return raw
	}
	text := systemText(raw)
	if text == "" {
		return nil
	}
	return marshalJSON(text)
}

func normalizeAnthropicContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return raw
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return raw
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		var typ string
		_ = json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			var text string
			_ = json.Unmarshal(b["text"], &text)
			out = append(out, map[string]any{"type": "text", "text": text})
		case "image":
			block := map[string]any{"type": "image"}
			if v, ok := rawJSONAny(b["source"]); ok {
				block["source"] = v
			}
			out = append(out, block)
		case "tool_use":
			block := map[string]any{"type": "tool_use"}
			copyRawJSONField(block, b, "id")
			copyRawJSONField(block, b, "name")
			copyRawJSONField(block, b, "input")
			out = append(out, block)
		case "tool_result":
			block := map[string]any{"type": "tool_result"}
			copyRawJSONField(block, b, "tool_use_id")
			copyAnthropicToolResultContent(block, b)
			copyRawJSONField(block, b, "is_error")
			out = append(out, block)
		}
	}
	if len(out) == 0 {
		return marshalJSON("")
	}
	return marshalJSON(out)
}

func copyAnthropicToolResultContent(dst map[string]any, src map[string]json.RawMessage) {
	if v, ok := rawJSONAny(src["content"]); ok {
		dst["content"] = truncateToolResultContent(v)
	}
}

func truncateToolResultContent(v any) any {
	remaining := maxAnthropicToolResultContentChars
	return truncateToolResultContentValue(v, &remaining)
}

func truncateToolResultContentValue(v any, remaining *int) any {
	switch x := v.(type) {
	case string:
		return truncateStringToBudget(x, remaining)
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			out = append(out, truncateToolResultContentValue(val, remaining))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = truncateToolResultContentValue(val, remaining)
		}
		return out
	default:
		return v
	}
}

func truncateStringToBudget(s string, remaining *int) string {
	if *remaining <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= *remaining {
		*remaining -= len(runes)
		return s
	}
	kept := *remaining
	*remaining = 0
	return string(runes[:kept]) + fmt.Sprintf("\n\n[ocgo truncated tool_result content: omitted %d characters]", len(runes)-kept)
}

func copyRawJSONField(dst map[string]any, src map[string]json.RawMessage, key string) {
	if v, ok := rawJSONAny(src[key]); ok {
		dst[key] = v
	}
}

func rawJSONAny(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	return stripCacheControl(v), true
}

func stripCacheControl(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "cache_control" {
				continue
			}
			out[k] = stripCacheControl(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			out = append(out, stripCacheControl(val))
		}
		return out
	default:
		return v
	}
}

func ensureAnthropicRequestDefaults(ar *AnthropicRequest) {
	ar.Model = resolveToolModel("claude", ar.Model)
	if ar.MaxTokens == 0 {
		ar.MaxTokens = 4096
	}
}

func resolveToolModel(tool, source string) string {
	mappings, err := loadModelMappings()
	if err != nil {
		mappings = defaultModelMappings()
	}
	return resolveMappedModel(tool, source, mappings)
}

func prepareChatBody(body []byte) ([]byte, error) {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body, nil
	}
	changed := requestStreamingUsage(req)
	if applyRawChatReasoningEffort(req) {
		changed = true
	}
	model, _ := req["model"].(string)
	if mapped := resolveToolModel("codex", model); mapped != model {
		req["model"] = mapped
		model = mapped
		changed = true
	}
	if rawChatBodyHasImages(req) {
		if !modelSupportsImages(model) {
			return nil, unsupportedImageModelError(model)
		}
		changed = stripRawChatImageDetails(req) || changed
	}
	if !changed {
		return sanitizeRawChatToolMessages(body), nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return sanitizeRawChatToolMessages(body), nil
	}
	return sanitizeRawChatToolMessages(out), nil
}

func applyRawChatReasoningEffort(req map[string]any) bool {
	effort := rawChatReasoningEffort(req)
	changed := false
	if effort != "" {
		current, _ := req["reasoning_effort"].(string)
		if current != effort {
			req["reasoning_effort"] = effort
			changed = true
		}
	}
	for _, key := range []string{"reasoning", "thinking", "effort", "level", "depth", "output_config"} {
		if _, ok := req[key]; ok {
			delete(req, key)
			changed = true
		}
	}
	return changed
}

func rawChatReasoningEffort(req map[string]any) string {
	for _, key := range []string{"reasoning_effort", "reasoning", "thinking", "output_config", "effort", "level", "depth"} {
		if effort := reasoningEffortFromAny(req[key]); effort != "" {
			return normalizeReasoningEffort(effort)
		}
	}
	return ""
}

func downstreamReasoningEffort(values ...json.RawMessage) string {
	for _, raw := range values {
		if effort := reasoningEffortFromRaw(raw); effort != "" {
			return normalizeReasoningEffort(effort)
		}
	}
	return ""
}

func reasoningEffortFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return reasoningEffortFromAny(v)
}

func reasoningEffortFromAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return formatReasoningNumber(t)
	case map[string]any:
		for _, key := range []string{"effort", "level", "depth", "reasoning_effort"} {
			if effort := reasoningEffortFromAny(t[key]); effort != "" {
				return effort
			}
		}
		if typ, _ := t["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "enabled") {
			return "high"
		}
		for _, key := range []string{"reasoning", "thinking", "output_config"} {
			if effort := reasoningEffortFromAny(t[key]); effort != "" {
				return effort
			}
		}
	}
	return ""
}

func formatReasoningNumber(n float64) string {
	if n == float64(int64(n)) {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

func normalizeReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "0", "minimal", "min", "none", "off", "disabled", "false":
		return "minimal"
	case "1", "low", "light":
		return "low"
	case "2", "medium", "med", "normal", "default":
		return "medium"
	case "3", "4", "high", "xhigh", "max", "maximum", "deep", "true", "enabled":
		return "high"
	default:
		return strings.TrimSpace(effort)
	}
}

func requestStreamingUsage(req map[string]any) bool {
	streaming, _ := req["stream"].(bool)
	if !streaming {
		return false
	}
	options, ok := req["stream_options"].(map[string]any)
	if !ok {
		options = map[string]any{}
		req["stream_options"] = options
	}
	if enabled, _ := options["include_usage"].(bool); enabled {
		return false
	}
	options["include_usage"] = true
	return true
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
	model := resolveToolModel("claude", ar.Model)
	out := OAIRequest{Model: model, Stream: ar.Stream, StreamOptions: streamUsageOptions(ar.Stream), MaxTokens: ar.MaxTokens, Temperature: ar.Temperature, TopP: ar.TopP, ReasoningEffort: downstreamReasoningEffort(ar.Reasoning, ar.Thinking, ar.OutputConfig, ar.ReasoningEffort, ar.Effort, ar.Level, ar.Depth)}
	if sys := systemText(ar.System); sys != "" {
		out.Messages = append(out.Messages, OAIMessage{Role: "system", Content: sys})
	}
	for _, m := range ar.Messages {
		out.Messages = append(out.Messages, contentToOpenAI(m)...)
	}
	for _, t := range ar.Tools {
		if strings.TrimSpace(t.Name) != "" {
			out.Tools = append(out.Tools, OAITool{Type: "function", Function: OAIFunction{Name: t.Name, Description: t.Description, Parameters: toolParametersOrDefault(t.InputSchema)}})
		}
	}
	return out
}

func responsesToChat(rr ResponsesRequest) OAIRequest {
	model := resolveToolModel("codex", rr.Model)
	out := OAIRequest{Model: model, Stream: rr.Stream, StreamOptions: streamUsageOptions(rr.Stream), MaxTokens: rr.MaxTokens, Temperature: rr.Temperature, TopP: rr.TopP, ReasoningEffort: downstreamReasoningEffort(rr.Reasoning, rr.Thinking, rr.OutputConfig, rr.ReasoningEffort, rr.Effort, rr.Level, rr.Depth)}
	if rr.Instructions != "" {
		out.Messages = append(out.Messages, OAIMessage{Role: "system", Content: rr.Instructions})
	}
	out.Messages = append(out.Messages, sanitizeOAIToolMessages(responsesInputToMessages(rr.Input))...)
	for _, t := range rr.Tools {
		if tool, ok := responseBuiltinToolToAnthropic(t); ok {
			out.AnthropicTools = appendUniqueAnthropicTool(out.AnthropicTools, tool)
			continue
		}
		if strings.TrimSpace(t.Name) != "" && (t.Type == "" || t.Type == "function") {
			out.Tools = append(out.Tools, OAITool{Type: "function", Function: OAIFunction{Name: t.Name, Description: t.Description, Parameters: toolParametersOrDefault(t.Parameters)}})
		}
	}
	return out
}

func responseBuiltinToolToAnthropic(t ResponseTool) (ATool, bool) {
	switch strings.ToLower(strings.TrimSpace(t.Type)) {
	case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11":
		tool := ATool{Type: "web_search_20250305", Name: "web_search", UserLocation: t.UserLocation}
		return tool, true
	case "web_fetch", "web_extractor":
		return ATool{Type: "web_fetch_20250910", Name: "web_fetch"}, true
	default:
		return ATool{}, false
	}
}

func appendUniqueAnthropicTool(tools []ATool, tool ATool) []ATool {
	for _, existing := range tools {
		if existing.Type == tool.Type && existing.Name == tool.Name {
			return tools
		}
	}
	return append(tools, tool)
}

func toolParametersOrDefault(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}

func chatToAnthropic(or OAIRequest) AnthropicRequest {
	model := resolveToolModel("codex", or.Model)
	out := AnthropicRequest{Model: model, MaxTokens: or.MaxTokens, Stream: or.Stream, Temperature: or.Temperature, TopP: or.TopP}
	if out.MaxTokens == 0 {
		out.MaxTokens = 4096
	}
	var system []string
	for _, m := range or.Messages {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		switch role {
		case "system":
			if text := openAIContentText(m.Content); text != "" {
				system = append(system, text)
			}
		case "tool":
			out.Messages = append(out.Messages, AMessage{Role: "user", Content: marshalJSON([]map[string]any{{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": openAIContentText(m.Content)}})})
		case "assistant":
			out.Messages = append(out.Messages, AMessage{Role: "assistant", Content: assistantContentToAnthropic(m)})
		default:
			if role == "" {
				role = "user"
			}
			out.Messages = append(out.Messages, AMessage{Role: role, Content: openAIContentToAnthropic(m.Content)})
		}
	}
	if len(system) > 0 {
		out.System = marshalJSON(strings.Join(system, "\n\n"))
	}
	for _, t := range or.AnthropicTools {
		out.Tools = appendUniqueAnthropicTool(out.Tools, t)
	}
	for _, t := range or.Tools {
		if strings.TrimSpace(t.Function.Name) != "" && (t.Type == "" || t.Type == "function") {
			out.Tools = append(out.Tools, ATool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: toolParametersOrDefault(t.Function.Parameters)})
		}
	}
	return out
}

func assistantContentToAnthropic(m OAIMessage) json.RawMessage {
	blocks := anthropicBlocksFromOpenAIContent(m.Content)
	for _, call := range m.ToolCalls {
		input := any(map[string]any{})
		if strings.TrimSpace(call.Function.Arguments) != "" {
			var parsed any
			if json.Unmarshal([]byte(call.Function.Arguments), &parsed) == nil {
				input = parsed
			} else {
				input = call.Function.Arguments
			}
		}
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
	}
	return marshalJSON(blocks)
}

func openAIContentToAnthropic(content any) json.RawMessage {
	if text, ok := content.(string); ok {
		return marshalJSON(text)
	}
	return marshalJSON(anthropicBlocksFromOpenAIContent(content))
}

func anthropicBlocksFromOpenAIContent(content any) []map[string]any {
	switch v := content.(type) {
	case nil:
		return []map[string]any{{"type": "text", "text": ""}}
	case string:
		if v == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": v}}
	case []OAIContentPart:
		var out []map[string]any
		for _, part := range v {
			out = appendAnthropicPart(out, part.Type, part.Text, part.ImageURL)
		}
		return out
	case []any:
		var out []map[string]any
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			text, _ := m["text"].(string)
			if text == "" {
				text, _ = m["output_text"].(string)
			}
			out = appendAnthropicPart(out, typ, text, imageURLFromAny(m["image_url"], m["url"]))
		}
		return out
	default:
		return []map[string]any{{"type": "text", "text": fmt.Sprint(v)}}
	}
}

func appendAnthropicPart(out []map[string]any, typ, text string, image *OAIImageURL) []map[string]any {
	switch typ {
	case "text", "input_text", "output_text":
		if text != "" {
			out = append(out, map[string]any{"type": "text", "text": text})
		}
	case "image_url", "input_image":
		if image != nil && image.URL != "" {
			out = append(out, map[string]any{"type": "image", "source": anthropicImageSource(image.URL)})
		}
	}
	return out
}

func imageURLFromAny(imageValue, urlValue any) *OAIImageURL {
	if s, ok := imageValue.(string); ok && s != "" {
		return &OAIImageURL{URL: s}
	}
	if m, ok := imageValue.(map[string]any); ok {
		if s, _ := m["url"].(string); s != "" {
			return &OAIImageURL{URL: s}
		}
	}
	if s, ok := urlValue.(string); ok && s != "" {
		return &OAIImageURL{URL: s}
	}
	return nil
}

func anthropicImageSource(url string) map[string]any {
	if strings.HasPrefix(url, "data:") {
		mediaType := "image/png"
		data := url
		if rest, ok := strings.CutPrefix(url, "data:"); ok {
			if header, body, found := strings.Cut(rest, ","); found {
				data = body
				if mt, _, found := strings.Cut(header, ";"); found && mt != "" {
					mediaType = mt
				}
			}
		}
		return map[string]any{"type": "base64", "media_type": mediaType, "data": data}
	}
	return map[string]any{"type": "url", "url": url}
}

func openAIContentText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []OAIContentPart:
		var b strings.Builder
		for _, part := range v {
			b.WriteString(part.Text)
		}
		return b.String()
	case []any:
		var b strings.Builder
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := m["text"].(string); text != "" {
				b.WriteString(text)
			}
			if text, _ := m["output_text"].(string); text != "" {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		return fmt.Sprint(v)
	}
}

func marshalJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func streamUsageOptions(streaming bool) *OAIStreamOptions {
	if !streaming {
		return nil
	}
	return &OAIStreamOptions{IncludeUsage: true}
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

const unavailableToolResultContent = "Tool result unavailable."

// sanitizeOAIToolMessages enforces the OpenAI-compatible invariant that an
// assistant message with tool_calls is immediately followed by tool messages
// for those exact call IDs. It drops orphan/late tool messages and inserts a
// conservative placeholder for any missing result.
func sanitizeOAIToolMessages(messages []OAIMessage) []OAIMessage {
	if len(messages) == 0 {
		return messages
	}
	out := make([]OAIMessage, 0, len(messages))
	for i := 0; i < len(messages); {
		m := messages[i]
		if m.Role == "tool" {
			// A tool message that is not consumed immediately after an assistant
			// tool_calls message is orphaned or late and must not be forwarded.
			i++
			continue
		}
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			i++
			continue
		}

		expected := toolCallIDOrder(m.ToolCalls)
		seen := map[string]bool{}
		j := i + 1
		for j < len(messages) && messages[j].Role == "tool" {
			toolMsg := messages[j]
			if containsString(expected, toolMsg.ToolCallID) && !seen[toolMsg.ToolCallID] {
				out = append(out, toolMsg)
				seen[toolMsg.ToolCallID] = true
			}
			j++
		}
		for _, id := range expected {
			if !seen[id] {
				out = append(out, OAIMessage{Role: "tool", ToolCallID: id, Content: unavailableToolResultContent})
			}
		}
		i = j
	}
	return out
}

func toolCallIDOrder(calls []OAIToolCall) []string {
	ids := make([]string, 0, len(calls))
	seen := map[string]bool{}
	for _, call := range calls {
		id := strings.TrimSpace(call.ID)
		if id != "" && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// sanitizeRawChatToolMessages sanitizes a raw chat-completions request while
// preserving unknown top-level and per-message fields. It only rebuilds the
// messages array when it actually needs to drop an orphan/late tool message or
// insert a placeholder for a missing result.
func sanitizeRawChatToolMessages(body []byte) []byte {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return body
	}
	rawMessages, ok := req["messages"]
	if !ok {
		return body
	}
	var messages []json.RawMessage
	if json.Unmarshal(rawMessages, &messages) != nil {
		return body
	}
	sanitized, changed := sanitizeRawChatMessages(messages)
	if !changed {
		return body
	}
	newMessages, err := json.Marshal(sanitized)
	if err != nil {
		return body
	}
	req["messages"] = newMessages
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

func sanitizeRawChatMessages(messages []json.RawMessage) ([]json.RawMessage, bool) {
	out := make([]json.RawMessage, 0, len(messages))
	changed := false
	for i := 0; i < len(messages); {
		msg := parseRawChatMessage(messages[i])
		if msg.Role == "tool" {
			changed = true
			i++
			continue
		}
		out = append(out, messages[i])
		if msg.Role != "assistant" || len(msg.ToolCallIDs) == 0 {
			i++
			continue
		}

		seen := map[string]bool{}
		j := i + 1
		for j < len(messages) {
			next := parseRawChatMessage(messages[j])
			if next.Role != "tool" {
				break
			}
			if containsString(msg.ToolCallIDs, next.ToolCallID) && !seen[next.ToolCallID] {
				out = append(out, messages[j])
				seen[next.ToolCallID] = true
			} else {
				changed = true
			}
			j++
		}
		for _, id := range msg.ToolCallIDs {
			if !seen[id] {
				out = append(out, rawToolPlaceholderMessage(id))
				changed = true
			}
		}
		i = j
	}
	return out, changed
}

type rawChatMessageInfo struct {
	Role        string
	ToolCallID  string
	ToolCallIDs []string
}

func parseRawChatMessage(raw json.RawMessage) rawChatMessageInfo {
	var msg struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	_ = json.Unmarshal(raw, &msg)
	info := rawChatMessageInfo{Role: msg.Role, ToolCallID: msg.ToolCallID}
	seen := map[string]bool{}
	for _, call := range msg.ToolCalls {
		id := strings.TrimSpace(call.ID)
		if id != "" && !seen[id] {
			info.ToolCallIDs = append(info.ToolCallIDs, id)
			seen[id] = true
		}
	}
	return info
}

func rawToolPlaceholderMessage(callID string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"role": "tool", "tool_call_id": callID, "content": unavailableToolResultContent})
	return b
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

type tokenUsage struct {
	InputTokens       int
	OutputTokens      int
	TotalTokens       int
	CachedInputTokens int
	Present           bool
}

func usageFromJSON(raw json.RawMessage) tokenUsage {
	var fields map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil {
		return tokenUsage{}
	}
	return usageFromFields(fields)
}

func usageFromAnyMap(v any) tokenUsage {
	fields, ok := v.(map[string]any)
	if !ok {
		return tokenUsage{}
	}
	return usageFromFields(fields)
}

func mergeUsage(a, b tokenUsage) tokenUsage {
	if !b.Present {
		return a
	}
	a.Present = true
	if b.InputTokens != 0 {
		a.InputTokens = b.InputTokens
	}
	if b.OutputTokens != 0 {
		a.OutputTokens = b.OutputTokens
	}
	if b.TotalTokens != 0 {
		a.TotalTokens = b.TotalTokens
	}
	if b.CachedInputTokens != 0 {
		a.CachedInputTokens = b.CachedInputTokens
	}
	if a.TotalTokens == 0 && (a.InputTokens > 0 || a.OutputTokens > 0) {
		a.TotalTokens = a.InputTokens + a.OutputTokens
	}
	return a
}

func usageFromFields(fields map[string]any) tokenUsage {
	if len(fields) == 0 {
		return tokenUsage{}
	}
	u := tokenUsage{Present: true}
	u.InputTokens = intField(fields, "prompt_tokens")
	if u.InputTokens == 0 {
		u.InputTokens = intField(fields, "input_tokens")
	}
	u.OutputTokens = intField(fields, "completion_tokens")
	if u.OutputTokens == 0 {
		u.OutputTokens = intField(fields, "output_tokens")
	}
	u.TotalTokens = intField(fields, "total_tokens")
	if u.TotalTokens == 0 && (u.InputTokens > 0 || u.OutputTokens > 0) {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	u.CachedInputTokens = cachedTokens(fields)
	return u
}

func intField(fields map[string]any, name string) int {
	v, ok := fields[name]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func cachedTokens(fields map[string]any) int {
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if nested, ok := fields[key].(map[string]any); ok {
			if n := intField(nested, "cached_tokens"); n > 0 {
				return n
			}
		}
	}
	if n := intField(fields, "cache_read_input_tokens"); n > 0 {
		return n
	}
	return intField(fields, "cached_tokens")
}

func anthropicUsage(u tokenUsage) map[string]int {
	usage := map[string]int{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens}
	if u.CachedInputTokens > 0 {
		usage["cache_read_input_tokens"] = u.CachedInputTokens
	}
	return usage
}

func anthropicDeltaUsage(u tokenUsage) map[string]int {
	usage := map[string]int{"output_tokens": u.OutputTokens}
	if u.InputTokens > 0 {
		usage["input_tokens"] = u.InputTokens
	}
	if u.CachedInputTokens > 0 {
		usage["cache_read_input_tokens"] = u.CachedInputTokens
	}
	return usage
}

func responsesUsage(u tokenUsage) map[string]any {
	usage := map[string]any{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "total_tokens": u.TotalTokens}
	if u.CachedInputTokens > 0 {
		usage["input_tokens_details"] = map[string]int{"cached_tokens": u.CachedInputTokens}
	}
	return usage
}

func openAIUsage(u tokenUsage) map[string]any {
	usage := map[string]any{"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.TotalTokens}
	if u.CachedInputTokens > 0 {
		usage["prompt_tokens_details"] = map[string]int{"cached_tokens": u.CachedInputTokens}
	}
	return usage
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
	usage := tokenUsage{}
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
		if chunk.Usage.Present {
			usage = chunk.Usage
		}
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
	usageJSON, _ := json.Marshal(anthropicDeltaUsage(usage))
	fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q,\"stop_sequence\":null},\"usage\":%s}\n\n", stopReason, usageJSON)
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
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.NewDecoder(body).Decode(&v)
	text := ""
	if len(v.Choices) > 0 {
		text = v.Choices[0].Message.Content
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "ocgo", "type": "message", "role": "assistant", "model": model, "content": []map[string]string{{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": anthropicUsage(usageFromJSON(v.Usage))})
}

type anthropicParsedResponse struct {
	Text      string
	ToolCalls []OAIToolCall
	Usage     tokenUsage
}

func parseAnthropicResponse(body io.Reader) anthropicParsedResponse {
	var v struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.NewDecoder(body).Decode(&v)
	out := anthropicParsedResponse{Usage: usageFromJSON(v.Usage)}
	var text strings.Builder
	for i, block := range v.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			id := block.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", i)
			}
			args := "{}"
			if len(block.Input) > 0 && string(block.Input) != "null" {
				args = string(block.Input)
			}
			out.ToolCalls = append(out.ToolCalls, OAIToolCall{ID: id, Type: "function", Function: OAICallFunction{Name: block.Name, Arguments: args}})
		}
	}
	out.Text = text.String()
	return out
}

func writeChatCompletionsResponseFromAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	parsed := parseAnthropicResponse(body)
	msg := map[string]any{"role": "assistant", "content": parsed.Text}
	finishReason := "stop"
	if len(parsed.ToolCalls) > 0 {
		msg["tool_calls"] = parsed.ToolCalls
		finishReason = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl_ocgo", "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finishReason}}, "usage": openAIUsage(parsed.Usage)})
}

func writeResponsesResponseFromAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	parsed := parseAnthropicResponse(body)
	var output []any
	if parsed.Text != "" || len(parsed.ToolCalls) == 0 {
		output = append(output, map[string]any{"id": "msg_ocgo", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": parsed.Text}}})
	}
	for _, call := range parsed.ToolCalls {
		output = append(output, map[string]any{"id": call.ID, "type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_ocgo", "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "completed", "output": output, "usage": responsesUsage(parsed.Usage)})
}

func streamChatCompletionsFromAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	writeChatCompletionChunk(w, model, map[string]any{"role": "assistant"}, nil)
	tools := map[int]streamedResponseToolCall{}
	readSSE(body, func(_ string, data []byte) bool {
		var v map[string]any
		if json.Unmarshal(data, &v) != nil {
			return true
		}
		typ, _ := v["type"].(string)
		switch typ {
		case "content_block_start":
			idx := intFromAny(v["index"])
			block, _ := v["content_block"].(map[string]any)
			if blockType, _ := block["type"].(string); blockType == "tool_use" {
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				if id == "" {
					id = fmt.Sprintf("call_%d", idx)
				}
				tools[idx] = streamedResponseToolCall{OutputIndex: len(tools), Call: OAIToolCall{ID: id, Type: "function", Function: OAICallFunction{Name: name}}}
				writeChatCompletionChunk(w, model, map[string]any{"tool_calls": []map[string]any{{"index": tools[idx].OutputIndex, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": ""}}}}, nil)
			}
		case "content_block_delta":
			idx := intFromAny(v["index"])
			delta, _ := v["delta"].(map[string]any)
			switch deltaType, _ := delta["type"].(string); deltaType {
			case "text_delta":
				if text, _ := delta["text"].(string); text != "" {
					writeChatCompletionChunk(w, model, map[string]any{"content": text}, nil)
				}
			case "input_json_delta":
				if tool, ok := tools[idx]; ok {
					part, _ := delta["partial_json"].(string)
					tool.Call.Function.Arguments += part
					tools[idx] = tool
					writeChatCompletionChunk(w, model, map[string]any{"tool_calls": []map[string]any{{"index": tool.OutputIndex, "function": map[string]any{"arguments": part}}}}, nil)
				}
			}
		case "message_stop":
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	})
	finish := "stop"
	if len(tools) > 0 {
		finish = "tool_calls"
	}
	writeChatCompletionChunk(w, model, map[string]any{}, &finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeChatCompletionChunk(w io.Writer, model string, delta map[string]any, finishReason *string) {
	choice := map[string]any{"index": 0, "delta": delta}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	}
	b, _ := json.Marshal(map[string]any{"id": "chatcmpl_ocgo", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{choice}})
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func streamResponsesFromAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	id := "resp_ocgo"
	writeResponseEvent(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "model": model, "status": "in_progress", "output": []any{}}})
	if flusher != nil {
		flusher.Flush()
	}
	messageStarted := false
	messageOutputIndex := -1
	nextOutputIndex := 0
	var text strings.Builder
	usage := tokenUsage{}
	blockToTool := map[int]int{}
	var tools []streamedResponseToolCall
	readSSE(body, func(_ string, data []byte) bool {
		var v map[string]any
		if json.Unmarshal(data, &v) != nil {
			return true
		}
		typ, _ := v["type"].(string)
		switch typ {
		case "message_start":
			if msg, _ := v["message"].(map[string]any); msg != nil {
				usage = mergeUsage(usage, usageFromAnyMap(msg["usage"]))
			}
		case "content_block_start":
			idx := intFromAny(v["index"])
			block, _ := v["content_block"].(map[string]any)
			switch blockType, _ := block["type"].(string); blockType {
			case "text":
				if !messageStarted {
					messageStarted = true
					messageOutputIndex = nextOutputIndex
					nextOutputIndex++
					writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_ocgo", "type": "message", "role": "assistant", "content": []any{}}})
					writeResponseEvent(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_ocgo", "output_index": messageOutputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
				}
			case "tool_use":
				callID, _ := block["id"].(string)
				name, _ := block["name"].(string)
				if callID == "" {
					callID = fmt.Sprintf("call_%d", idx)
				}
				toolPos := len(tools)
				blockToTool[idx] = toolPos
				outputIndex := nextOutputIndex
				nextOutputIndex++
				tools = append(tools, streamedResponseToolCall{OutputIndex: outputIndex, Call: OAIToolCall{ID: callID, Type: "function", Function: OAICallFunction{Name: name}}})
				writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": map[string]any{"id": callID, "type": "function_call", "call_id": callID, "name": name, "arguments": ""}})
			}
		case "content_block_delta":
			idx := intFromAny(v["index"])
			delta, _ := v["delta"].(map[string]any)
			switch deltaType, _ := delta["type"].(string); deltaType {
			case "text_delta":
				if part, _ := delta["text"].(string); part != "" {
					if !messageStarted {
						messageStarted = true
						messageOutputIndex = nextOutputIndex
						nextOutputIndex++
						writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_ocgo", "type": "message", "role": "assistant", "content": []any{}}})
						writeResponseEvent(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_ocgo", "output_index": messageOutputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
					}
					text.WriteString(part)
					writeResponseEvent(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_ocgo", "output_index": messageOutputIndex, "content_index": 0, "delta": part})
				}
			case "input_json_delta":
				toolPos, ok := blockToTool[idx]
				if ok {
					part, _ := delta["partial_json"].(string)
					tools[toolPos].Call.Function.Arguments += part
					writeResponseEvent(w, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": tools[toolPos].Call.ID, "output_index": tools[toolPos].OutputIndex, "delta": part})
				}
			}
		case "message_delta":
			usage = mergeUsage(usage, usageFromAnyMap(v["usage"]))
		case "message_stop":
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	})
	var output []any
	if messageStarted {
		writeResponseEvent(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_ocgo", "output_index": messageOutputIndex, "content_index": 0, "text": text.String()})
		writeResponseEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_ocgo", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text.String()}}}})
		output = append(output, map[string]any{"id": "msg_ocgo", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text.String()}}})
	}
	for _, tool := range tools {
		call := tool.Call
		item := map[string]any{"id": call.ID, "type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments}
		writeResponseEvent(w, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": call.ID, "output_index": tool.OutputIndex, "arguments": call.Function.Arguments})
		writeResponseEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tool.OutputIndex, "item": item})
		output = append(output, item)
	}
	writeResponseEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "model": model, "status": "completed", "output": output, "usage": responsesUsage(usage)}})
}

func readSSE(body io.Reader, handle func(event string, data []byte) bool) {
	s := bufio.NewScanner(body)
	var event string
	var data []string
	flush := func() bool {
		if len(data) == 0 {
			return true
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			return false
		}
		return handle(event, []byte(payload))
	}
	for s.Scan() {
		line := strings.TrimRight(s.Text(), "\r")
		if line == "" {
			if !flush() {
				return
			}
			event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	_ = flush()
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
	usage := tokenUsage{}
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
		if chunk.Usage.Present {
			usage = chunk.Usage
		}
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
	writeResponseEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "model": model, "status": "completed", "output": output, "usage": responsesUsage(usage)}})
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
	Usage            tokenUsage
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
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(data, &v)
	out := openAIStreamChunk{Usage: usageFromJSON(v.Usage)}
	if len(v.Choices) == 0 {
		return out
	}
	delta := v.Choices[0].Delta
	out.Content = delta.Content
	out.ReasoningContent = delta.ReasoningContent
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
		Usage json.RawMessage `json:"usage"`
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
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_ocgo", "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "completed", "output": output, "usage": responsesUsage(usageFromJSON(v.Usage))})
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

var modelMappingFile = func() string { return filepath.Join(configDir(), "model-mapping.json") }

func codexConfigFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

func codexProfileConfigFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", codexProfileName+".config.toml")
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
	profilePath := filepath.Join(filepath.Dir(path), codexProfileName+".config.toml")
	catalogPath := codexModelCatalogFile()
	profileText := strings.Join([]string{
		fmt.Sprintf("openai_base_url = %q", baseURL),
		`forced_login_method = "api"`,
		fmt.Sprintf("model_provider = %q", codexProfileName),
		fmt.Sprintf("model_catalog_json = %q", catalogPath),
		`model_reasoning_effort = "minimal"`,
		`model_reasoning_summary = "none"`,
		"",
		fmt.Sprintf("[model_providers.%s]", codexProfileName),
		`name = "OpenCode Go"`,
		fmt.Sprintf("base_url = %q", baseURL),
		`wire_api = "responses"`,
		"",
	}, "\n")
	if err := os.WriteFile(profilePath, []byte(profileText), 0644); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	text := ""
	if err == nil {
		text = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cleaned := stripLegacyCodexProfile(text)
	return os.WriteFile(path, []byte(cleaned), 0644)
}

func stripLegacyCodexProfile(text string) string {
	var out []string
	inRemovedSection := false
	currentSection := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			currentSection = trimmed
			inRemovedSection = isLegacyCodexProfileSection(currentSection)
			if inRemovedSection {
				continue
			}
		}
		if inRemovedSection {
			continue
		}
		if currentSection == "" && strings.HasPrefix(trimmed, "profile") {
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[0]) == "profile" && strings.Trim(strings.TrimSpace(parts[1]), `"'`) == codexProfileName {
				continue
			}
		}
		out = append(out, line)
	}
	return strings.TrimLeft(strings.Join(out, "\n"), "\n")
}

func isLegacyCodexProfileSection(section string) bool {
	profiles := fmt.Sprintf("[profiles.%s", codexProfileName)
	providers := fmt.Sprintf("[model_providers.%s", codexProfileName)
	return section == fmt.Sprintf("[profiles.%s]", codexProfileName) ||
		strings.HasPrefix(section, profiles+".") ||
		section == fmt.Sprintf("[model_providers.%s]", codexProfileName) ||
		strings.HasPrefix(section, providers+".")
}

func writeCodexModelCatalog(path string) error {
	mappings, err := loadModelMappings()
	if err != nil {
		mappings = defaultModelMappings()
	}
	models := make([]map[string]any, 0, len(knownModelIDs())+len(mappings["codex"]))
	seen := map[string]bool{}
	addModel := func(id, target, description string, i int) {
		if seen[id] {
			return
		}
		seen[id] = true
		meta := modelMetadata(target)
		displayName := id
		if id == target {
			displayName = meta.DisplayName
		}
		models = append(models, map[string]any{
			"slug":                             id,
			"display_name":                     displayName,
			"description":                      description,
			"default_reasoning_level":          meta.DefaultReasoningLevel,
			"supported_reasoning_levels":       meta.SupportedReasoning,
			"shell_type":                       "shell_command",
			"visibility":                       "list",
			"supported_in_api":                 true,
			"priority":                         i,
			"availability_nux":                 nil,
			"upgrade":                          nil,
			"base_instructions":                "You are Codex, a coding agent running in a terminal-based coding assistant.",
			"supports_reasoning_summaries":     meta.ReasoningSummaries,
			"default_reasoning_summary":        meta.DefaultReasoningSummary,
			"support_verbosity":                false,
			"default_verbosity":                nil,
			"apply_patch_tool_type":            nil,
			"web_search_tool_type":             "text",
			"truncation_policy":                map[string]any{"mode": "tokens", "limit": 10000},
			"supports_parallel_tool_calls":     meta.ParallelToolCalls,
			"supports_image_detail_original":   meta.SupportsImageOriginal,
			"context_window":                   meta.ContextWindow,
			"max_context_window":               meta.MaxContextWindow,
			"auto_compact_token_limit":         nil,
			"effective_context_window_percent": 95,
			"experimental_supported_tools":     []any{},
			"input_modalities":                 meta.CodexInputModalities,
			"supports_search_tool":             meta.SupportsSearchTool,
		})
	}
	for i, id := range knownModelIDs() {
		addModel(id, id, modelMetadata(id).Description, i)
	}
	keys := make([]string, 0, len(mappings["codex"]))
	for source := range mappings["codex"] {
		keys = append(keys, source)
	}
	sort.Strings(keys)
	for i, source := range keys {
		target := mappings["codex"][source]
		addModel(source, target, "OCGO mapping to "+target, len(knownModelIDs())+i)
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
