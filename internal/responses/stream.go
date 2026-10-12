package responses

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// TranslatorOptions configures a StreamTranslator.
type TranslatorOptions struct {
	// Writer receives the Responses SSE stream.
	Writer io.Writer
	// Model is reported in the response object.
	Model string
	// ResponseID is the id of the response. Empty means generate one.
	ResponseID string
	// ReasoningSummary emits a reasoning output item carrying upstream
	// reasoning text. Codex only displays it when the model catalog advertises
	// reasoning summaries, so it is off by default.
	ReasoningSummary bool
	// Warn receives translation warnings, one per occurrence.
	Warn func(string)
}

// StreamTranslator converts a chat-completions SSE stream into the Responses
// SSE event sequence Codex expects.
//
// It is a state machine rather than a one-shot converter because the upstream
// stream is genuinely incremental: tool-call arguments arrive split across
// chunks, can be interleaved across parallel calls, and may be split mid-JSON.
type StreamTranslator struct {
	opts   TranslatorOptions
	seq    int
	respID string
	silent bool

	// items holds every output item in the order it was created, which is also
	// its output_index order.
	items []*streamItem
	// open tracks the item currently receiving deltas, per kind.
	reasoning *streamItem
	message   *streamItem
	// tools maps the upstream tool_call index to its item, because parallel
	// calls interleave.
	tools map[int]*streamItem

	usage   Usage
	created bool
	done    bool
}

// streamItem is one output item under construction.
type streamItem struct {
	id     string
	index  int
	kind   string
	text   strings.Builder
	callID string
	name   string
	args   strings.Builder
	closed bool
}

// NewStreamTranslator returns a translator writing to opts.Writer.
func NewStreamTranslator(opts TranslatorOptions) *StreamTranslator {
	if opts.ResponseID == "" {
		opts.ResponseID = newResponseID()
	}
	return &StreamTranslator{opts: opts, respID: opts.ResponseID, tools: map[int]*streamItem{}}
}

// Usage reports the usage seen so far.
func (t *StreamTranslator) Usage() Usage { return t.usage }

// Handle consumes one chat-completions SSE data payload.
func (t *StreamTranslator) Handle(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "[DONE]" {
		return nil
	}
	var chunk struct {
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
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		t.warn("ignored an unparseable upstream chunk")
		return nil
	}
	if u := usageFromJSON(chunk.Usage); u.Present {
		t.usage = u
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	delta := chunk.Choices[0].Delta
	if delta.ReasoningContent != "" {
		t.appendReasoning(delta.ReasoningContent)
	}
	if delta.Content != "" {
		t.appendText(delta.Content)
	}
	for _, call := range delta.ToolCalls {
		t.appendToolCall(call.Index, call.ID, call.Function.Name, call.Function.Arguments)
	}
	return nil
}

// Finish closes any open items and emits response.completed.
func (t *StreamTranslator) Finish() error {
	if t.done {
		return nil
	}
	t.done = true
	t.closeAll()
	return t.emit("response.completed", map[string]any{
		"response": map[string]any{
			"id":         t.respID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"model":      t.opts.Model,
			"status":     "completed",
			"output":     t.outputItems(),
			"usage":      t.usage.Payload(),
		},
	})
}

// Fail emits a response.failed event. Codex surfaces the message and treats a
// rate-limit or server failure as retryable.
func (t *StreamTranslator) Fail(code, message string) error {
	if t.done {
		return nil
	}
	t.done = true
	if !t.created {
		if err := t.ensureCreated(); err != nil {
			return err
		}
	}
	return t.emit("response.failed", map[string]any{
		"response": map[string]any{
			"id":         t.respID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"model":      t.opts.Model,
			"status":     "failed",
			"output":     []any{},
			"error":      map[string]any{"code": code, "message": message},
		},
	})
}

func (t *StreamTranslator) appendReasoning(text string) {
	if !t.opts.ReasoningSummary {
		return
	}
	if t.reasoning == nil || t.reasoning.closed {
		t.closeOpenMessage()
		t.reasoning = t.newItem("reasoning", "rs_"+t.respID)
		_ = t.emit("response.output_item.added", map[string]any{
			"output_index": t.reasoning.index,
			"item":         map[string]any{"id": t.reasoning.id, "type": "reasoning", "summary": []any{}},
		})
		_ = t.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": t.reasoning.id, "output_index": t.reasoning.index, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	t.reasoning.text.WriteString(text)
	_ = t.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": t.reasoning.id, "output_index": t.reasoning.index, "summary_index": 0,
		"delta": text,
	})
}

func (t *StreamTranslator) appendText(text string) {
	if t.message == nil || t.message.closed {
		t.closeOpenReasoning()
		t.message = t.newItem("message", "msg_"+t.respID)
		_ = t.emit("response.output_item.added", map[string]any{
			"output_index": t.message.index,
			"item": map[string]any{
				"id": t.message.id, "type": "message", "role": "assistant", "status": "in_progress",
				"content": []any{},
			},
		})
		_ = t.emit("response.content_part.added", map[string]any{
			"item_id": t.message.id, "output_index": t.message.index, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	t.message.text.WriteString(text)
	_ = t.emit("response.output_text.delta", map[string]any{
		"item_id": t.message.id, "output_index": t.message.index, "content_index": 0,
		"delta": text,
	})
}

func (t *StreamTranslator) appendToolCall(index int, id, name, args string) {
	item, ok := t.tools[index]
	if !ok {
		// A tool call closes the text item: the Responses API keeps output
		// items sequential, and Codex reads the completed item list in order.
		t.closeOpenMessage()
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", t.respID, index)
		}
		item = t.newItem("function_call", id)
		item.callID = id
		t.tools[index] = item
		_ = t.emit("response.output_item.added", map[string]any{
			"output_index": item.index,
			"item": map[string]any{
				"id": item.id, "type": "function_call", "status": "in_progress",
				"call_id": item.callID, "name": name, "arguments": "",
			},
		})
	}
	if id != "" {
		item.callID = id
		item.id = id
	}
	if name != "" {
		item.name = name
	}
	if args != "" {
		item.args.WriteString(args)
		_ = t.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": item.id, "output_index": item.index, "delta": args,
		})
	}
}

// closeAll closes every open item, in output_index order.
func (t *StreamTranslator) closeAll() {
	for _, item := range t.items {
		t.closeItem(item)
	}
}

func (t *StreamTranslator) closeOpenReasoning() {
	if t.reasoning != nil {
		t.closeItem(t.reasoning)
	}
}

func (t *StreamTranslator) closeOpenMessage() {
	if t.message != nil {
		t.closeItem(t.message)
	}
}

func (t *StreamTranslator) closeItem(item *streamItem) {
	if item == nil || item.closed {
		return
	}
	item.closed = true
	switch item.kind {
	case "reasoning":
		text := item.text.String()
		_ = t.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": item.id, "output_index": item.index, "summary_index": 0, "text": text,
		})
		_ = t.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": item.id, "output_index": item.index, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": text},
		})
	case "message":
		text := item.text.String()
		_ = t.emit("response.output_text.done", map[string]any{
			"item_id": item.id, "output_index": item.index, "content_index": 0, "text": text,
		})
		_ = t.emit("response.content_part.done", map[string]any{
			"item_id": item.id, "output_index": item.index, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
	case "function_call":
		args := item.args.String()
		// The client parses arguments as JSON, so a truncated stream would
		// otherwise fail far from its cause. Report it here and still emit the
		// raw text so nothing is silently lost.
		if args != "" && !json.Valid([]byte(args)) {
			t.warn(fmt.Sprintf("tool call %s arguments are not valid JSON; forwarding raw text", item.name))
		}
		_ = t.emit("response.function_call_arguments.done", map[string]any{
			"item_id": item.id, "output_index": item.index, "arguments": args,
		})
	}
	_ = t.emit("response.output_item.done", map[string]any{
		"output_index": item.index, "item": item.payload(),
	})
}

func (i *streamItem) payload() map[string]any {
	switch i.kind {
	case "reasoning":
		return map[string]any{
			"id": i.id, "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": i.text.String()}},
		}
	case "function_call":
		return map[string]any{
			"id": i.id, "type": "function_call", "status": "completed",
			"call_id": i.callID, "name": i.name, "arguments": i.args.String(),
		}
	default:
		return map[string]any{
			"id": i.id, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": i.text.String(), "annotations": []any{}}},
		}
	}
}

// newItem creates the next output item. Creation order is output order, so the
// index is simply the position in items.
func (t *StreamTranslator) newItem(kind, id string) *streamItem {
	item := &streamItem{id: id, index: len(t.items), kind: kind}
	t.items = append(t.items, item)
	return item
}

func (t *StreamTranslator) outputItems() []any {
	out := make([]any, 0, len(t.items))
	for _, item := range t.items {
		out = append(out, item.payload())
	}
	return out
}

func (t *StreamTranslator) ensureCreated() error {
	if t.created {
		return nil
	}
	t.created = true
	response := map[string]any{
		"id": t.respID, "object": "response", "created_at": time.Now().Unix(),
		"model": t.opts.Model, "status": "in_progress", "output": []any{},
	}
	if err := t.emit("response.created", map[string]any{"response": response}); err != nil {
		return err
	}
	return t.emit("response.in_progress", map[string]any{"response": response})
}

func (t *StreamTranslator) emit(name string, payload map[string]any) error {
	if err := t.ensureCreated(); err != nil {
		return err
	}
	if t.silent {
		return nil
	}
	t.seq++
	payload["type"] = name
	payload["sequence_number"] = t.seq
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(t.opts.Writer, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if f, ok := t.opts.Writer.(interface{ Flush() }); ok {
		f.Flush()
	}
	return nil
}

func (t *StreamTranslator) warn(msg string) {
	if t.opts.Warn != nil {
		t.opts.Warn(msg)
	}
}

func newResponseID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	return "resp_" + hex.EncodeToString(b[:])
}

// TranslateStream reads a chat-completions SSE stream and writes the equivalent
// Responses event stream.
func TranslateStream(w io.Writer, body io.Reader, opts TranslatorOptions) (*StreamTranslator, error) {
	opts.Writer = w
	tr := NewStreamTranslator(opts)
	if err := tr.ensureCreated(); err != nil {
		return tr, err
	}
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if err := tr.Handle([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))); err != nil {
			return tr, err
		}
	}
	if err := s.Err(); err != nil {
		return tr, err
	}
	return tr, tr.Finish()
}

// TranslateResponse converts a non-streaming chat completion into a single
// Responses response object.
func TranslateResponse(w io.Writer, body io.Reader, opts TranslatorOptions) (*StreamTranslator, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	var v struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("invalid upstream response: %w", err)
	}
	opts.Writer = w
	tr := NewStreamTranslator(opts)
	// The non-streaming path answers with one JSON object, so the incremental
	// event writes are suppressed and only the bookkeeping runs.
	tr.silent = true
	if u := usageFromJSON(v.Usage); u.Present {
		tr.usage = u
	}
	if len(v.Choices) > 0 {
		msg := v.Choices[0].Message
		if msg.ReasoningContent != "" {
			tr.appendReasoning(msg.ReasoningContent)
		}
		if msg.Content != "" {
			tr.appendText(msg.Content)
		}
		for i, call := range msg.ToolCalls {
			tr.appendToolCall(i, call.ID, call.Function.Name, call.Function.Arguments)
		}
	}
	tr.closeAll()
	tr.done = true
	out := map[string]any{
		"id": tr.respID, "object": "response", "created_at": time.Now().Unix(),
		"model": opts.Model, "status": "completed",
		"output": tr.outputItems(),
		"usage":  tr.usage.Payload(),
	}
	b, err := json.Marshal(out)
	if err != nil {
		return tr, err
	}
	if _, err := w.Write(b); err != nil {
		return tr, err
	}
	return tr, nil
}
