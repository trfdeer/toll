package admin

import "testing"

func TestRequestMessagesFormats(t *testing.T) {
	// chat/completions with multimodal content and a tool call.
	got := requestMessages(`{"model":"m","messages":[` +
		`{"role":"system","content":"be nice"},` +
		`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"x"}}]},` +
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"result"}]}`)
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(got), got)
	}
	if got[1].Content != "look\n[image]" {
		t.Errorf("multimodal content = %q", got[1].Content)
	}
	if len(got[2].ToolCalls) != 1 || got[2].ToolCalls[0].Name != "search" ||
		got[2].ToolCalls[0].Arguments != `{"q":"x"}` {
		t.Errorf("tool call not normalized: %+v", got[2].ToolCalls)
	}
	if got[3].ToolCallID != "call_1" || got[3].Content != "result" {
		t.Errorf("tool result not normalized: %+v", got[3])
	}

	// responses API: instructions, a function call + its output, and reason.
	got = requestMessages(`{"instructions":"sys","input":[` +
		`{"type":"message","role":"user","content":"hello"},` +
		`{"type":"function_call","call_id":"c9","name":"lookup","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c9","output":"value"},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]}]}`)
	if len(got) != 5 {
		t.Fatalf("responses messages = %d, want 5: %+v", len(got), got)
	}
	if got[0].Role != "system" || got[1].Content != "hello" {
		t.Errorf("instructions/message wrong: %+v", got[:2])
	}
	if len(got[2].ToolCalls) != 1 || got[2].ToolCalls[0].ID != "c9" || got[2].ToolCalls[0].Name != "lookup" {
		t.Errorf("responses function_call wrong: %+v", got[2])
	}
	if got[3].Role != "tool" || got[3].ToolCallID != "c9" || got[3].Content != "value" {
		t.Errorf("responses function_call_output wrong: %+v", got[3])
	}
	if got[4].Role != "reasoning" || got[4].Content != "thought" {
		t.Errorf("responses reasoning wrong: %+v", got[4])
	}
}

func TestResponseMessagesShapes(t *testing.T) {
	cases := map[string]string{
		// Non-streaming chat.completion.
		`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`: "hi",
		// Streamed assembler output (bare message object).
		`{"model":"m","role":"assistant","content":"streamed"}`: "streamed",
		// responses format.
		`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responded"}]}]}`: "responded",
	}
	for raw, want := range cases {
		got := responseMessages(raw)
		if len(got) != 1 || got[0].Role != "assistant" || got[0].Content != want {
			t.Errorf("responseMessages(%s) = %+v, want assistant %q", raw, got, want)
		}
	}

	// Empty / in-flight response has no messages.
	if got := responseMessages(""); got != nil {
		t.Errorf("empty response = %+v, want nil", got)
	}

	// Reasoning is carried across, under either provider spelling.
	for _, raw := range []string{
		`{"choices":[{"message":{"role":"assistant","content":"hi","reasoning":"why"}}]}`,
		`{"role":"assistant","content":"hi","reasoning_content":"why"}`,
	} {
		got := responseMessages(raw)
		if len(got) != 1 || got[0].Reasoning != "why" {
			t.Errorf("reasoning not extracted from %s: %+v", raw, got)
		}
	}
}

func TestResponseFinishReasons(t *testing.T) {
	// chat finish_reason lives on the choice.
	got := responseMessages(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","reasoning":"thinking"}}]}`)
	if len(got) != 1 || got[0].FinishReason != "length" || got[0].Content != "" {
		t.Errorf("chat finish_reason = %+v", got)
	}

	// responses: incomplete maps to the underlying reason; completed is normal.
	got = responseMessages(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]}`)
	if len(got) != 1 || got[0].FinishReason != "max_output_tokens" {
		t.Errorf("responses finish_reason = %+v", got)
	}
	got = responseMessages(`{"status":"completed","output":[{"type":"message","role":"assistant","content":"done"}]}`)
	if len(got) != 1 || got[0].FinishReason != "" {
		t.Errorf("completed should have no finish badge: %+v", got)
	}

	// responses output function call becomes a structured tool call.
	got = responseMessages(`{"status":"completed","output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`)
	if len(got) != 1 || len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != "c1" {
		t.Errorf("responses tool call = %+v", got)
	}
}
