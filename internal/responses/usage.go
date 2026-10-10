package responses

import "encoding/json"

// Usage is the token accounting reported on a completed response. Codex
// decides when to compact the conversation from these numbers, so a bridge that
// reports zero never compacts and silently overflows the context window.
type Usage struct {
	Input     int
	Output    int
	Total     int
	Cached    int
	Reasoning int
	Present   bool
}

// Payload renders the usage object for a Responses event.
func (u Usage) Payload() map[string]any {
	out := map[string]any{
		"input_tokens":  u.Input,
		"output_tokens": u.Output,
		"total_tokens":  u.Total,
	}
	if u.Cached > 0 {
		out["input_tokens_details"] = map[string]any{"cached_tokens": u.Cached}
	}
	if u.Reasoning > 0 {
		out["output_tokens_details"] = map[string]any{"reasoning_tokens": u.Reasoning}
	}
	return out
}

// usageFromJSON reads a chat-completions usage object. Both the OpenAI names
// and the nested detail objects are accepted, and either may be absent.
func usageFromJSON(raw json.RawMessage) Usage {
	if len(raw) == 0 || string(raw) == "null" {
		return Usage{}
	}
	var v struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		InputTokens         int `json:"input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		InputTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return Usage{}
	}
	u := Usage{
		Input:     firstNonZero(v.PromptTokens, v.InputTokens),
		Output:    firstNonZero(v.CompletionTokens, v.OutputTokens),
		Total:     v.TotalTokens,
		Reasoning: detailReasoning(v.CompletionTokensDetails),
	}
	if d := v.PromptTokensDetails; d != nil {
		u.Cached = d.CachedTokens
	}
	if d := v.InputTokensDetails; d != nil {
		u.Cached = firstNonZero(d.CachedTokens, u.Cached)
	}
	if u.Total == 0 {
		u.Total = u.Input + u.Output
	}
	u.Present = u.Input != 0 || u.Output != 0 || u.Total != 0
	return u
}

func detailReasoning(d *struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}) int {
	if d == nil {
		return 0
	}
	return d.ReasoningTokens
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// EstimateTokens approximates a token count for a request body. It is used by
// /v1/messages/count_tokens, which must never return zero: clients use the
// figure to decide when to compact, and a zero answer disables that.
//
// The estimate is deliberately simple — one token per four bytes of JSON, with
// a floor of one — because the alternative is an empty answer, and because the
// real number is reported by the upstream usage block on every response.
func EstimateTokens(body []byte) int {
	if len(body) == 0 {
		return 1
	}
	n := len(body) / 4
	if n < 1 {
		return 1
	}
	return n
}
