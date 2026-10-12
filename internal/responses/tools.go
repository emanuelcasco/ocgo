package responses

import (
	"encoding/json"
	"strings"
)

// Tool is one entry of the Responses `tools` array.
//
// Only the fields the bridge acts on are modelled; anything else is ignored
// rather than rejected, so a client adding a field cannot break translation.
type Tool struct {
	Type              string          `json:"type"`
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	Parameters        json.RawMessage `json:"parameters"`
	Strict            *bool           `json:"strict"`
	Format            json.RawMessage `json:"format"`
	Tools             []Tool          `json:"tools"`
	ExternalWebAccess *bool           `json:"external_web_access"`
}

// hostedToolTypes are Responses tool types served by the API provider itself
// rather than by the model, so they cannot be expressed as chat functions.
var hostedToolTypes = map[string]string{
	"web_search":                    "web_search",
	"web_search_preview":            "web_search",
	"web_search_2025_08_26":         "web_search",
	"web_search_preview_2025_03_11": "web_search",
	"file_search":                   "file_search",
	"computer_use_preview":          "computer_use",
	"code_interpreter":              "code_interpreter",
	"web_fetch":                     "web_fetch",
	"web_extractor":                 "web_fetch",
	"image_generation":              "image_generation",
	"mcp":                           "mcp",
}

// chatTools converts a Responses tools array into chat function tools.
//
// It returns the chat tools, the hosted tool kinds the caller must serve by
// other means, and a warning per entry that could not be represented.
//
// A `namespace` tool is skipped deliberately. Flattening its inner functions
// would produce names the client cannot map back to the namespace, so a call
// would round-trip as an unknown tool; skipping makes the sub-agent tools
// unavailable instead of broken.
func chatTools(in []Tool) (tools []ChatTool, hosted []string, warnings []string) {
	for _, t := range in {
		kind := strings.ToLower(strings.TrimSpace(t.Type))
		if k, ok := hostedToolTypes[kind]; ok {
			hosted = appendUnique(hosted, k)
			continue
		}
		switch kind {
		case "", "function":
			if strings.TrimSpace(t.Name) == "" {
				warnings = append(warnings, "skipped a function tool with no name")
				continue
			}
			tools = append(tools, ChatTool{
				Type: "function",
				Function: ChatFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  parametersOrDefault(t.Parameters),
				},
			})
		case "custom":
			if strings.TrimSpace(t.Name) == "" {
				warnings = append(warnings, "skipped a custom tool with no name")
				continue
			}
			desc := t.Description
			if len(t.Format) > 0 {
				desc = strings.TrimSpace(desc + " Input is freeform and is passed through verbatim in the `input` field.")
			}
			tools = append(tools, ChatTool{
				Type: "function",
				Function: ChatFunction{
					Name:        t.Name,
					Description: desc,
					Parameters:  FreeformInputSchema,
				},
			})
			warnings = append(warnings, "custom tool "+t.Name+" exposed as a function with a single `input` string")
		case "namespace":
			warnings = append(warnings, "namespace tool "+t.Name+" dropped: its inner tools cannot round-trip through chat completions")
		default:
			warnings = append(warnings, "unknown tool type "+t.Type+" ("+t.Name+") skipped")
		}
	}
	return tools, hosted, warnings
}

func parametersOrDefault(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return EmptyObjectSchema
	}
	return raw
}

func appendUnique(values []string, want string) []string {
	for _, v := range values {
		if v == want {
			return values
		}
	}
	return append(values, want)
}
