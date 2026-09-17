package wire

import (
	"encoding/json"
	"strings"
)

// ChatStreamAssembler reassembles a streamed chat.completion into a single
// assistant message: text deltas are concatenated, reasoning deltas are kept
// separate, tool-call argument fragments are accumulated per tool-call index,
// and the finish reason is tracked. Only choice 0 is assembled (n=1 clients;
// multi-choice streams still record usage correctly via the usage chunk).
type ChatStreamAssembler struct {
	content      strings.Builder
	reasoning    strings.Builder
	role         string
	finishReason string
	toolCalls    map[int]*assembledToolCall
	sawChoice    bool
}

type assembledToolCall struct {
	ID   string
	Name string
	Args strings.Builder
}

type deltaChunk struct {
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          any    `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
}

// Feed consumes one parsed chat.completion.chunk object.
func (a *ChatStreamAssembler) Feed(obj map[string]json.RawMessage) {
	var chunk deltaChunk
	if json.Unmarshal(obj["choices"], &chunk.Choices) != nil || len(chunk.Choices) == 0 {
		return // usage-only final chunk or non-chat object
	}
	a.sawChoice = true
	c := chunk.Choices[0]

	if c.Delta.Role != "" {
		a.role = c.Delta.Role
	}
	if s, ok := c.Delta.Content.(string); ok {
		a.content.WriteString(s)
	}
	// Thinking models stream their chain-of-thought alongside (or instead of)
	// the answer. Providers spell the field either way; take whichever is set.
	switch {
	case c.Delta.ReasoningContent != "":
		a.reasoning.WriteString(c.Delta.ReasoningContent)
	case c.Delta.Reasoning != "":
		a.reasoning.WriteString(c.Delta.Reasoning)
	}
	for _, tc := range c.Delta.ToolCalls {
		if a.toolCalls == nil {
			a.toolCalls = map[int]*assembledToolCall{}
		}
		slot, exists := a.toolCalls[tc.Index]
		if !exists {
			slot = &assembledToolCall{}
			a.toolCalls[tc.Index] = slot
		}
		if tc.ID != "" {
			slot.ID = tc.ID
		}
		if tc.Function.Name != "" {
			slot.Name = tc.Function.Name
		}
		slot.Args.WriteString(tc.Function.Arguments)
	}
	if fr, ok := c.FinishReason.(string); ok && fr != "" {
		a.finishReason = fr
	}
}

// Build renders the assembled assistant message. Returns ok=false when no
// choice chunks were ever fed (usage-only stream).
func (a *ChatStreamAssembler) Build(model string) (json.RawMessage, bool) {
	if !a.sawChoice {
		return nil, false
	}
	msg := map[string]any{
		"model":   model,
		"role":    orDefault(a.role, "assistant"),
		"content": a.content.String(),
	}
	if a.reasoning.Len() > 0 {
		msg["reasoning_content"] = a.reasoning.String()
	}
	if len(a.toolCalls) > 0 {
		calls := make([]map[string]any, 0, len(a.toolCalls))
		for i := 0; i < len(a.toolCalls); i++ {
			tc := a.toolCalls[i]
			if tc == nil {
				continue
			}
			call := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Args.String(),
				},
			}
			if tc.ID != "" {
				call["id"] = tc.ID
			}
			calls = append(calls, call)
		}
		msg["tool_calls"] = calls
	}
	if a.finishReason != "" {
		msg["finish_reason"] = a.finishReason
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return nil, false
	}
	return out, true
}

// ResponseObjectFrom extracts the full response object from a
// responses-format event (response.completed carries the final response).
// Returns nil when the object is not such an event.
func ResponseObjectFrom(obj map[string]json.RawMessage) json.RawMessage {
	var t string
	json.Unmarshal(obj["type"], &t)
	if !strings.HasPrefix(t, "response.completed") {
		return nil
	}
	if ro := obj["response"]; len(ro) > 0 && json.Valid(ro) {
		return ro
	}
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
