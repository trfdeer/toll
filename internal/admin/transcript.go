package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trfdeer/toll/internal/store"
)

// toolCall is a normalized function call: the wire formats nest the name and
// arguments differently (chat uses function.{name,arguments}; the responses
// API puts them at the top level).
type toolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// chatMessage is one rendered turn in a request's conversation. content is
// flattened to text (multimodal parts become their text, images a placeholder)
// so the UI can render chat bubbles without knowing every upstream shape.
type chatMessage struct {
	Role         string     `json:"role"`
	Content      string     `json:"content"`
	Reasoning    string     `json:"reasoning,omitempty"`
	Name         string     `json:"name,omitempty"`
	ToolCallID   string     `json:"toolCallId,omitempty"`
	ToolCalls    []toolCall `json:"toolCalls,omitempty"`
	FinishReason string     `json:"finishReason,omitempty"`
}

// requestDetail is GET /admin/api/requests/{id}: a stored transcript
// normalized into a conversation plus its metadata.
type requestDetail struct {
	ID             int64  `json:"id"`
	ConversationID string `json:"conversationId"`
	GatewayModel   string `json:"gatewayModel"`
	UpstreamModel  string `json:"upstreamModel"`
	Status         int    `json:"status"`
	CreatedAt      string `json:"createdAt"`
	CompletedAt    string `json:"completedAt"`
	DurationMS     *int64 `json:"durationMs"`
	// ContentStored is false when prompt storage is disabled, so the UI can
	// say so instead of showing an empty conversation.
	ContentStored bool          `json:"contentStored"`
	Messages      []chatMessage `json:"messages"`
}

// requestDetail serves one transcript as a chat conversation. Both the
// request and response blobs are parsed defensively: an unrecognized shape
// yields an empty conversation rather than an error.
func (h *handlers) requestDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	t, err := h.store.Transcript(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrTranscriptNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "request not found"})
			return
		}
		h.fail(w, err, "request unavailable")
		return
	}
	// Bodies count as stored when they exist, even if prompt storage has since
	// been disabled; the flag only drives the "storage disabled" notice.
	contentStored := h.store.PromptsEnabled() || t.RequestJSON != "" || t.ResponseJSON != ""
	writeJSON(w, http.StatusOK, buildRequestDetail(t, contentStored))
}

func buildRequestDetail(t store.TranscriptRow, contentStored bool) requestDetail {
	return requestDetail{
		ID:             t.ID,
		ConversationID: t.ConversationID,
		GatewayModel:   t.GatewayModel,
		UpstreamModel:  t.UpstreamModel,
		Status:         t.Status,
		CreatedAt:      t.CreatedAt,
		CompletedAt:    t.CompletedAt,
		DurationMS:     transcriptDuration(t),
		ContentStored:  contentStored,
		Messages:       transcriptMessages(t),
	}
}

// transcriptDuration is completed_at - created_at in milliseconds, or nil
// while the request is still in flight.
func transcriptDuration(t store.TranscriptRow) *int64 {
	if t.CompletedAt == "" {
		return nil
	}
	start, err1 := time.Parse(time.RFC3339Nano, t.CreatedAt)
	end, err2 := time.Parse(time.RFC3339Nano, t.CompletedAt)
	if err1 != nil || err2 != nil {
		return nil
	}
	ms := end.Sub(start).Milliseconds()
	return &ms
}

// transcriptMessages assembles the request turns plus the assistant reply.
func transcriptMessages(t store.TranscriptRow) []chatMessage {
	msgs := requestMessages(t.RequestJSON)
	msgs = append(msgs, responseMessages(t.ResponseJSON)...)
	return msgs
}

// requestMessages normalizes a request body: chat/completions carries
// "messages", the responses API carries "input" (+ "instructions").
func requestMessages(raw string) []chatMessage {
	if raw == "" {
		return nil
	}
	var req map[string]any
	if json.Unmarshal([]byte(raw), &req) != nil {
		return nil
	}
	if arr, ok := req["messages"].([]any); ok {
		return messageArray(arr)
	}

	var out []chatMessage
	if s, ok := req["instructions"].(string); ok && s != "" {
		out = append(out, chatMessage{Role: "system", Content: s})
	}
	switch input := req["input"].(type) {
	case string:
		if input != "" {
			out = append(out, chatMessage{Role: "user", Content: input})
		}
	case []any:
		for _, item := range input {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch str(m["type"]) {
			case "function_call":
				out = append(out, chatMessage{
					Role:      "assistant",
					ToolCalls: functionCallTools(m),
				})
			case "function_call_output":
				out = append(out, chatMessage{
					Role:       "tool",
					ToolCallID: str(m["call_id"]),
					Content:    contentString(m["output"]),
				})
			case "reasoning":
				out = append(out, chatMessage{
					Role:    "reasoning",
					Content: firstNonEmpty(contentString(m["summary"]), contentString(m["content"])),
				})
			default:
				out = append(out, chatMessage{
					Role:    orAssistant(str(m["role"])),
					Content: contentString(m["content"]),
					Name:    str(m["name"]),
				})
			}
		}
	}
	return out
}

// responseMessages normalizes a stored response: a non-streaming
// chat.completion (choices[0].message), a streamed message object (the
// assembler's output), or a responses-format object ("output").
func responseMessages(raw string) []chatMessage {
	if raw == "" {
		return nil
	}
	var resp map[string]any
	if json.Unmarshal([]byte(raw), &resp) != nil {
		return nil
	}
	if choices, ok := resp["choices"].([]any); ok {
		if len(choices) == 0 {
			return nil
		}
		c, ok := choices[0].(map[string]any)
		if !ok {
			return nil
		}
		m, ok := c["message"].(map[string]any)
		if !ok {
			return nil
		}
		msg := messageFromMap(m)
		msg.FinishReason = firstNonEmpty(str(c["finish_reason"]), msg.FinishReason)
		return []chatMessage{msg}
	}
	if _, ok := resp["role"]; ok {
		// Streamed chat: the assembler stored a bare message object.
		return []chatMessage{messageFromMap(resp)}
	}
	if arr, ok := resp["output"].([]any); ok {
		out := responseOutput(arr)
		if n := len(out); n > 0 {
			out[n-1].FinishReason = firstNonEmpty(out[n-1].FinishReason, responsesFinish(resp))
		}
		return out
	}
	return nil
}

// responseOutput walks a responses-format "output" array.
func responseOutput(arr []any) []chatMessage {
	out := make([]chatMessage, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch str(m["type"]) {
		case "function_call":
			out = append(out, chatMessage{Role: "assistant", ToolCalls: functionCallTools(m)})
		case "reasoning":
			out = append(out, chatMessage{
				Role:    "reasoning",
				Content: firstNonEmpty(contentString(m["summary"]), contentString(m["content"])),
			})
		default:
			out = append(out, chatMessage{Role: orAssistant(str(m["role"])), Content: contentString(m["content"])})
		}
	}
	return out
}

// responsesFinish maps a responses-format status to a chat-style finish
// reason. A normal "completed" response carries no badge.
func responsesFinish(resp map[string]any) string {
	if str(resp["status"]) != "incomplete" {
		return ""
	}
	if d, ok := resp["incomplete_details"].(map[string]any); ok {
		if r := str(d["reason"]); r != "" {
			return r
		}
	}
	return "incomplete"
}

// functionCallTools extracts a single tool call from a responses-format
// function_call item.
func functionCallTools(m map[string]any) []toolCall {
	name := str(m["name"])
	args := str(m["arguments"])
	if name == "" && args == "" {
		return nil
	}
	return []toolCall{{
		ID:        firstNonEmpty(str(m["call_id"]), str(m["id"])),
		Name:      name,
		Arguments: args,
	}}
}

// messageArray converts a chat/completions "messages" array.
func messageArray(arr []any) []chatMessage {
	out := make([]chatMessage, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, messageFromMap(m))
	}
	return out
}

func messageFromMap(m map[string]any) chatMessage {
	return chatMessage{
		Role:         str(m["role"]),
		Content:      contentString(m["content"]),
		Reasoning:    firstNonEmpty(str(m["reasoning"]), str(m["reasoning_content"])),
		Name:         str(m["name"]),
		ToolCallID:   str(m["tool_call_id"]),
		ToolCalls:    parseToolCalls(m["tool_calls"]),
		FinishReason: str(m["finish_reason"]),
	}
}

// parseToolCalls normalizes the chat "tool_calls" array (and tolerates the
// responses item shape) into toolCall values.
func parseToolCalls(v any) []toolCall {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]toolCall, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		tc := toolCall{
			ID:        firstNonEmpty(str(m["id"]), str(m["call_id"])),
			Name:      str(m["name"]),
			Arguments: str(m["arguments"]),
		}
		if fn, ok := m["function"].(map[string]any); ok {
			tc.Name = firstNonEmpty(str(fn["name"]), tc.Name)
			tc.Arguments = firstNonEmpty(str(fn["arguments"]), tc.Arguments)
		}
		if tc.Name == "" && tc.Arguments == "" {
			continue
		}
		out = append(out, tc)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// firstNonEmpty returns the first non-empty string, for providers that spell
// the same field differently (reasoning vs reasoning_content).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// contentString flattens a message content value to text. Content is a string
// for most requests but an array of typed parts for multimodal ones.
func contentString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, part := range t {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(s)
				continue
			}
			switch str(m["type"]) {
			case "image_url", "input_image":
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString("[image]")
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func orAssistant(role string) string {
	if role == "" {
		return "assistant"
	}
	return role
}
