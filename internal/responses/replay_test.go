package responses

import (
	"encoding/json"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// fixturesDir holds sanitised traces captured from a real Codex CLI session
// driven through the bridge. Regenerate with scripts/makefixtures.py.
const fixturesDir = "../../testdata/fixtures"

var generatedID = regexp.MustCompile(`resp_[A-Za-z0-9_]{4,}`)

func fixtureNames(t *testing.T, kind string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(fixturesDir, kind))
	if err != nil {
		t.Skipf("no fixtures available: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sse") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Skip("no fixtures available")
	}
	return names
}

func readFixture(t *testing.T, kind, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir, kind, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// normalise removes the fields that legitimately differ between runs: generated
// response ids, timestamps and sequence numbers. Everything else must match.
func normalise(v any) any {
	switch value := v.(type) {
	case string:
		return generatedID.ReplaceAllString(value, "resp_GENERATED")
	case []any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, normalise(item))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, item := range value {
			switch k {
			case "sequence_number", "created_at":
				continue
			}
			out[k] = normalise(item)
		}
		return out
	default:
		return v
	}
}

func normalisedEvents(t *testing.T, sse string) []string {
	t.Helper()
	var out []string
	for _, e := range parseEvents(t, sse) {
		payload, err := json.Marshal(normalise(e.Data))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Name+" "+string(payload))
	}
	return out
}

// TestFixtureReplayReproducesRecordedEvents is the regression guard for the
// translator: replaying a recorded upstream stream must produce exactly the
// event stream Codex was served during the live session.
func TestFixtureReplayReproducesRecordedEvents(t *testing.T) {
	for _, name := range fixtureNames(t, "chat_stream") {
		t.Run(name, func(t *testing.T) {
			upstream := readFixture(t, "chat_stream", name)
			recorded, err := os.ReadFile(filepath.Join(fixturesDir, "codex_events", name))
			if err != nil {
				t.Skipf("no recorded events for %s", name)
			}
			got := normalisedEvents(t, translateToString(t, upstream))
			want := normalisedEvents(t, string(recorded))
			if len(got) != len(want) {
				t.Fatalf("event count: got %d, want %d\n got %v\nwant %v", len(got), len(want), got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("event %d differs:\n got %s\nwant %s", i, got[i], want[i])
				}
			}
		})
	}
}

func translateToString(t *testing.T, upstream string) string {
	t.Helper()
	var out strings.Builder
	opts := TranslatorOptions{Model: "glm-5.3-flash", ResponseID: "resp_fixture"}
	if _, err := TranslateStream(&out, strings.NewReader(upstream), opts); err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	return out.String()
}

// completedToolArguments returns the assembled arguments of every tool call in
// the response.completed event, keyed by call id.
func completedToolArguments(t *testing.T, sse string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range parseEvents(t, sse) {
		if e.Name != "response.completed" {
			continue
		}
		response, ok := e.Data["response"].(map[string]any)
		if !ok {
			continue
		}
		for _, item := range response["output"].([]any) {
			call, ok := item.(map[string]any)
			if !ok || call["type"] != "function_call" {
				continue
			}
			id, _ := call["call_id"].(string)
			args, _ := call["arguments"].(string)
			out[id] = args
		}
	}
	return out
}

// TestFixtureChunkSplitsReassembleIdentically splits each recorded upstream
// stream at every small boundary and at randomised boundaries, and requires the
// reassembled tool-call arguments to be identical every time. The live mock
// splits arguments mid-JSON, so a reassembly bug here is a real bug.
func TestFixtureChunkSplitsReassembleIdentically(t *testing.T) {
	for _, name := range fixtureNames(t, "chat_stream") {
		t.Run(name, func(t *testing.T) {
			upstream := readFixture(t, "chat_stream", name)
			want := completedToolArguments(t, translateToString(t, upstream))

			for _, size := range []int{1, 2, 3, 5, 7, 13, 64} {
				got := completedToolArguments(t, translateChunked(t, upstream, size))
				assertSameArguments(t, want, got, "fixed chunk size "+strconv.Itoa(size))
			}
			rng := rand.New(rand.NewPCG(1, 2))
			for i := 0; i < 20; i++ {
				got := completedToolArguments(t, translateRandomChunks(t, upstream, rng))
				assertSameArguments(t, want, got, "random splits")
			}
		})
	}
}

func assertSameArguments(t *testing.T, want, got map[string]string, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d tool calls, want %d (%v vs %v)", label, len(got), len(want), got, want)
	}
	for id, wantArgs := range want {
		if got[id] != wantArgs {
			t.Fatalf("%s: call %s arguments\n got %q\nwant %q", label, id, got[id], wantArgs)
		}
	}
}

type fixedReader struct {
	data []byte
	size int
}

func (r *fixedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.size
	if n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

type randomReader struct {
	data []byte
	rng  *rand.Rand
}

func (r *randomReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := 1 + r.rng.IntN(9)
	if n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func translateChunked(t *testing.T, upstream string, size int) string {
	t.Helper()
	var out strings.Builder
	r := &fixedReader{data: []byte(upstream), size: size}
	if _, err := TranslateStream(&out, r, TranslatorOptions{Model: "m", ResponseID: "resp_fixture"}); err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	return out.String()
}

func translateRandomChunks(t *testing.T, upstream string, rng *rand.Rand) string {
	t.Helper()
	var out strings.Builder
	r := &randomReader{data: []byte(upstream), rng: rng}
	if _, err := TranslateStream(&out, r, TranslatorOptions{Model: "m", ResponseID: "resp_fixture"}); err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	return out.String()
}

// FuzzToolCallChunkSplits is the fuzz target: any split of a recorded stream
// must reassemble to the same tool-call arguments. The seed corpus is the
// recorded fixtures, so plain `go test` already exercises real data.
func FuzzToolCallChunkSplits(f *testing.F) {
	entries, err := os.ReadDir(filepath.Join(fixturesDir, "chat_stream"))
	if err != nil {
		f.Skip("no fixtures available")
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sse") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(fixturesDir, "chat_stream", e.Name()))
		if err != nil {
			continue
		}
		f.Add(string(b), uint8(3))
	}

	f.Fuzz(func(t *testing.T, upstream string, chunk uint8) {
		size := int(chunk%16) + 1
		baseline := completedToolArguments(t, translateToString(t, upstream))
		if len(baseline) == 0 {
			t.Skip("no tool calls in this input")
		}
		got := completedToolArguments(t, translateChunked(t, upstream, size))
		assertSameArguments(t, baseline, got, "chunked replay")
	})
}
