// Package wire implements parsing and rewriting for the OpenAI-compatible
// wire formats proxied by the gateway: chat/completions and responses.
//
// Per the usage findings (HYPERGW page): cached_tokens lives at
// usage.prompt_tokens_details.cached_tokens (chat) or
// usage.input_tokens_details.cached_tokens (responses) and is OMITTED on
// cache miss — a missing field means 0. Reasoning tokens live at
// usage.completion_tokens_details.reasoning_tokens (chat) or
// usage.output_tokens_details.reasoning_tokens (responses) and are also
// omitted when there was none.
package wire

import (
	"encoding/json"
	"fmt"
)

// Usage is the normalized token accounting extracted from either format.
type Usage struct {
	PromptTokens    int
	CompletionToken int
	CachedTokens    int
	ReasoningTokens int
	UpstreamCostUSD *float64 // upstream-reported cost, when the provider includes one
}

// ParseRequest extracts the gateway-facing model and stream flag from a
// request body (both formats carry "model" and "stream" at the top level).
func ParseRequest(body []byte) (model string, stream bool, err error) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", false, fmt.Errorf("parse request body: %w", err)
	}
	if req.Model == "" {
		return "", false, fmt.Errorf("request body has no model")
	}
	return req.Model, req.Stream, nil
}

// RewriteModel replaces the top-level "model" field in a JSON body. Used on
// the way in (alias → upstream ID) and on the way out (upstream ID → alias).
// Returns the input unchanged when there is no model field.
func RewriteModel(body []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("rewrite model: %w", err)
	}
	if _, ok := obj["model"]; !ok {
		return body, nil
	}
	var err error
	obj["model"], err = json.Marshal(model)
	if err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}

// ExtractUsage pulls normalized usage from a JSON object along with the
// raw usage JSON it was found in. Handles:
//   - chat/completions responses and final stream chunks (usage at top level)
//   - responses-format events (usage inside "response", e.g.
//     the response.completed event)
//
// Returns (usage, raw, false) when the object carries no usage at all.
func ExtractUsage(obj map[string]json.RawMessage) (Usage, json.RawMessage, bool) {
	raw, ok := obj["usage"]
	if !ok {
		// responses-format events (e.g. response.completed) nest the whole
		// response object, usage inside it.
		var inner map[string]json.RawMessage
		if json.Unmarshal(obj["response"], &inner) == nil {
			usageRaw, hasUsage := inner["usage"]
			if hasUsage {
				var m map[string]json.RawMessage
				if json.Unmarshal(usageRaw, &m) == nil {
					u, _, found := extractUsageMap(m)
					if found {
						return u, usageRaw, true
					}
				}
			}
		}
		return Usage{}, nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return Usage{}, nil, false
	}
	u, _, found := extractUsageMap(m)
	return u, raw, found
}

// ExtractUsageFromBytes is a convenience wrapper for raw JSON.
func ExtractUsageFromBytes(data []byte) (Usage, json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return Usage{}, nil, false
	}
	return ExtractUsage(obj)
}

// extractUsageMap handles both naming schemes in one place. Returns the
// usage map's own JSON as the raw form.
func extractUsageMap(m map[string]json.RawMessage) (Usage, json.RawMessage, bool) {
	u := Usage{}
	found := false

	if v, err := number(m, "prompt_tokens"); err == nil {
		u.PromptTokens = v
		found = true
	}
	if v, err := number(m, "input_tokens"); err == nil {
		u.PromptTokens = v
		found = true
	}
	if v, err := number(m, "completion_tokens"); err == nil {
		u.CompletionToken = v
		found = true
	}
	if v, err := number(m, "output_tokens"); err == nil {
		u.CompletionToken = v
		found = true
	}
	if v, ok := detailNumber(m, "prompt_tokens_details", "cached_tokens"); ok {
		u.CachedTokens = v
		found = true
	}
	if v, ok := detailNumber(m, "input_tokens_details", "cached_tokens"); ok {
		u.CachedTokens = v
		found = true
	}
	if v, ok := detailNumber(m, "completion_tokens_details", "reasoning_tokens"); ok {
		u.ReasoningTokens = v
		found = true
	}
	if v, ok := detailNumber(m, "output_tokens_details", "reasoning_tokens"); ok {
		u.ReasoningTokens = v
		found = true
	}
	// Upstream-reported cost (e.g. Hyper's usage.cost.usd).
	if cost, ok := m["cost"]; ok {
		var c struct {
			USD *float64 `json:"usd"`
		}
		if json.Unmarshal(cost, &c) == nil {
			u.UpstreamCostUSD = c.USD
			found = found || c.USD != nil
		}
	}
	if !found {
		return Usage{}, nil, false
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return Usage{}, nil, false
	}
	return u, raw, true
}

func number(m map[string]json.RawMessage, key string) (int, error) {
	raw, ok := m[key]
	if !ok {
		return 0, fmt.Errorf("no %s", key)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, err
	}
	return int(f), nil
}

// detailNumber reads m[details][field]; a missing details object or field is
// not an error — it means 0 (cache miss / no reasoning).
func detailNumber(m map[string]json.RawMessage, details, field string) (int, bool) {
	raw, ok := m[details]
	if !ok {
		return 0, false
	}
	var d map[string]json.RawMessage
	if json.Unmarshal(raw, &d) != nil {
		return 0, false
	}
	v, err := number(d, field)
	if err != nil {
		return 0, false
	}
	return v, true
}
