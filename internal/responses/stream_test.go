package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

type emittedEvent struct {
	Name string
	Data map[string]any
}

// runStream feeds an SSE body through TranslateStream and parses the events
// that came back out.
func runStream(t *testing.T, sse string, opts TranslatorOptions) ([]emittedEvent, []string) {
	t.Helper()
	var warnings []string
	opts.Warn = func(msg string) { warnings = append(warnings, msg) }
	if opts.ResponseID == "" {
		opts.ResponseID = "resp_test"
	}
	var out strings.Builder
	if _, err := TranslateStream(&out, strings.NewReader(sse), opts); err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	return parseEvents(t, out.String()), warnings
}

func parseEvents(t *testing.T, sse string) []emittedEvent {
	t.Helper()
	var events []emittedEvent
	for _, block := range strings.Split(sse, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			t.Fatalf("event block without a name: %q", block)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("bad event payload %q: %v", data, err)
		}
		events = append(events, emittedEvent{Name: name, Data: payload})
	}
	return events
}

func names(events []emittedEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Name)
	}
	return out
}

func eventNamed(t *testing.T, events []emittedEvent, name string) emittedEvent {
	t.Helper()
	for _, e := range events {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no %s event in %v", name, names(events))
	return emittedEvent{}
}

func textStream(text string) string {
	return `data: {"choices":[{"delta":{"content":` + mustJSON(text) + `}}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13,"prompt_tokens_details":{"cached_tokens":3}}}` + "\n\n" +
		"data: [DONE]\n\n"
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestStreamTextEventOrder(t *testing.T) {
	events, _ := runStream(t, textStream("hi"), TranslatorOptions{Model: "glm-5.3-flash"})
	got := strings.Join(names(events), ",")
	want := strings.Join([]string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}, ",")
	if got != want {
		t.Fatalf("event order:\n got %s\nwant %s", got, want)
	}
}

func TestStreamToolCallEventOrder(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	events, _ := runStream(t, sse, TranslatorOptions{Model: "glm-5.3-flash"})
	got := strings.Join(names(events), ",")
	want := strings.Join([]string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}, ",")
	if got != want {
		t.Fatalf("event order:\n got %s\nwant %s", got, want)
	}
	for _, name := range names(events) {
		if strings.Contains(name, "output_text") {
			t.Fatalf("tool-only stream emitted text events: %v", names(events))
		}
	}
	done := eventNamed(t, events, "response.function_call_arguments.done")
	if done.Data["arguments"] != `{"cmd":"pwd"}` {
		t.Fatalf("arguments reassembled as %v", done.Data["arguments"])
	}
}

func TestStreamInterleavedParallelToolCalls(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"one","arguments":"{\"x\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"two","arguments":"{\"y\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	events, warnings := runStream(t, sse, TranslatorOptions{Model: "m"})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	completed := eventNamed(t, events, "response.completed")
	output := completed.Data["response"].(map[string]any)["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected two output items, got %d", len(output))
	}
	first := output[0].(map[string]any)
	second := output[1].(map[string]any)
	if first["call_id"] != "call_a" || first["arguments"] != `{"x":1}` {
		t.Fatalf("first call wrong: %+v", first)
	}
	if second["call_id"] != "call_b" || second["arguments"] != `{"y":2}` {
		t.Fatalf("second call wrong: %+v", second)
	}
	if first["id"] == second["id"] {
		t.Fatalf("tool call item ids must be unique: %v", first["id"])
	}
}

func TestStreamReportsUsageOnCompleted(t *testing.T) {
	events, _ := runStream(t, textStream("hi"), TranslatorOptions{Model: "m"})
	completed := eventNamed(t, events, "response.completed")
	usage := completed.Data["response"].(map[string]any)["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 9 || usage["output_tokens"].(float64) != 4 || usage["total_tokens"].(float64) != 13 {
		t.Fatalf("usage wrong: %+v", usage)
	}
	details := usage["input_tokens_details"].(map[string]any)
	if details["cached_tokens"].(float64) != 3 {
		t.Fatalf("cached tokens wrong: %+v", details)
	}
}

func TestStreamWarnsOnInvalidToolArgumentsButForwardsThem(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"exec_command","arguments":"{\"cmd\":"}}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	events, warnings := runStream(t, sse, TranslatorOptions{Model: "m"})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not valid JSON") {
		t.Fatalf("expected an invalid-JSON warning, got %v", warnings)
	}
	done := eventNamed(t, events, "response.function_call_arguments.done")
	if done.Data["arguments"] != `{"cmd":` {
		t.Fatalf("raw arguments should be forwarded, got %v", done.Data["arguments"])
	}
}

func TestStreamSequenceNumbersAreMonotonic(t *testing.T) {
	events, _ := runStream(t, textStream("hi"), TranslatorOptions{Model: "m"})
	last := 0.0
	for _, e := range events {
		seq, ok := e.Data["sequence_number"].(float64)
		if !ok {
			t.Fatalf("%s has no sequence_number", e.Name)
		}
		if seq <= last {
			t.Fatalf("sequence_number %v did not advance past %v at %s", seq, last, e.Name)
		}
		last = seq
		if e.Data["type"] != e.Name {
			t.Fatalf("payload type %v does not match event name %s", e.Data["type"], e.Name)
		}
	}
}

func TestStreamTextAndToolCallsGetDistinctItems(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"thinking out loud"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"exec_command","arguments":"{}"}}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	events, _ := runStream(t, sse, TranslatorOptions{Model: "m"})
	completed := eventNamed(t, events, "response.completed")
	output := completed.Data["response"].(map[string]any)["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected a message then a call, got %+v", output)
	}
	if output[0].(map[string]any)["type"] != "message" {
		t.Fatalf("first item = %+v", output[0])
	}
	if output[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("second item = %+v", output[1])
	}
	// The text item must be closed before the call opens, or Codex sees a
	// completed item interleaved with a live one.
	textDone := indexOf(names(events), "response.output_text.done")
	if textDone == -1 {
		t.Fatal("text item was never closed")
	}
	callAdded := -1
	seenAdded := 0
	for i, name := range names(events) {
		if name != "response.output_item.added" {
			continue
		}
		seenAdded++
		if seenAdded == 2 {
			callAdded = i
			break
		}
	}
	if callAdded == -1 {
		t.Fatal("tool call item was never added")
	}
	if textDone > callAdded {
		t.Fatalf("text item was still open when the call started: %v", names(events))
	}
}

func indexOf(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}

func TestStreamReasoningSummaryOffByDefault(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_content":"weighing options","content":"done"}}]}` + "\n\n" + "data: [DONE]\n\n"
	events, _ := runStream(t, sse, TranslatorOptions{Model: "m"})
	for _, name := range names(events) {
		if strings.Contains(name, "reasoning") {
			t.Fatalf("reasoning events emitted while disabled: %v", names(events))
		}
	}
}

func TestStreamReasoningSummaryWhenEnabled(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"reasoning_content":"weighing "}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"reasoning_content":"options"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"done"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	events, _ := runStream(t, sse, TranslatorOptions{Model: "m", ReasoningSummary: true})
	if indexOf(names(events), "response.reasoning_summary_text.delta") == -1 {
		t.Fatalf("reasoning deltas missing: %v", names(events))
	}
	completed := eventNamed(t, events, "response.completed")
	output := completed.Data["response"].(map[string]any)["output"].([]any)
	reasoning := output[0].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Fatalf("first output item should be reasoning: %+v", reasoning)
	}
	summary := reasoning["summary"].([]any)[0].(map[string]any)
	if summary["text"] != "weighing options" {
		t.Fatalf("reasoning summary = %v", summary["text"])
	}
}

func TestFailEmitsResponseFailed(t *testing.T) {
	var out strings.Builder
	tr := NewStreamTranslator(TranslatorOptions{Writer: &out, Model: "m", ResponseID: "resp_test"})
	if err := tr.Fail("upstream_429", "rate limited"); err != nil {
		t.Fatal(err)
	}
	events := parseEvents(t, out.String())
	last := events[len(events)-1]
	if last.Name != "response.failed" {
		t.Fatalf("last event = %s, want response.failed", last.Name)
	}
	response := last.Data["response"].(map[string]any)
	if response["status"] != "failed" {
		t.Fatalf("status = %v", response["status"])
	}
	errObj := response["error"].(map[string]any)
	if errObj["code"] != "upstream_429" || errObj["message"] != "rate limited" {
		t.Fatalf("error = %+v", errObj)
	}
}

func TestFinishIsIdempotent(t *testing.T) {
	var out strings.Builder
	tr := NewStreamTranslator(TranslatorOptions{Writer: &out, Model: "m", ResponseID: "resp_test"})
	if err := tr.Handle([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := tr.Finish(); err != nil {
		t.Fatal(err)
	}
	first := out.String()
	if err := tr.Finish(); err != nil {
		t.Fatal(err)
	}
	if out.String() != first {
		t.Fatal("second Finish emitted more events")
	}
}

func TestHandleIgnoresDoneAndGarbage(t *testing.T) {
	var out strings.Builder
	tr := NewStreamTranslator(TranslatorOptions{Writer: &out, Model: "m", ResponseID: "resp_test", Warn: func(string) {}})
	for _, payload := range []string{"[DONE]", "", "   ", "not json at all"} {
		if err := tr.Handle([]byte(payload)); err != nil {
			t.Fatalf("Handle(%q): %v", payload, err)
		}
	}
	if err := tr.Finish(); err != nil {
		t.Fatal(err)
	}
	events := parseEvents(t, out.String())
	if events[len(events)-1].Name != "response.completed" {
		t.Fatalf("stream should still complete cleanly: %v", names(events))
	}
}

func TestHandleWarnsOnGarbage(t *testing.T) {
	var warnings []string
	tr := NewStreamTranslator(TranslatorOptions{
		Writer: &strings.Builder{}, Model: "m", ResponseID: "resp_test",
		Warn: func(msg string) { warnings = append(warnings, msg) },
	})
	_ = tr.Handle([]byte("not json at all"))
	if len(warnings) != 1 {
		t.Fatalf("expected one warning, got %v", warnings)
	}
}

func TestTranslateResponseProducesOneJSONObject(t *testing.T) {
	body := `{"choices":[{"message":{"content":"done","reasoning_content":"hmm","tool_calls":[{"id":"call_a","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}]}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	var out strings.Builder
	if _, err := TranslateResponse(&out, strings.NewReader(body), TranslatorOptions{Model: "m", ResponseID: "resp_test"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "event: ") {
		t.Fatalf("non-streaming output must not contain SSE frames:\n%s", text)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, text)
	}
	if parsed["status"] != "completed" {
		t.Fatalf("status = %v", parsed["status"])
	}
	output := parsed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected a message and a call, got %+v", output)
	}
	usage := parsed["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 7 || usage["output_tokens"].(float64) != 3 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestStreamEmptyBodyStillCompletes(t *testing.T) {
	events, _ := runStream(t, "", TranslatorOptions{Model: "m"})
	if len(events) != 3 {
		t.Fatalf("expected created, in_progress and completed, got %v", names(events))
	}
	completed := eventNamed(t, events, "response.completed")
	output := completed.Data["response"].(map[string]any)["output"].([]any)
	if len(output) != 0 {
		t.Fatalf("expected no output items, got %+v", output)
	}
}
