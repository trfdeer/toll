package wire

import (
	"encoding/json"
	"testing"
)

func feed(t *testing.T, a *ChatStreamAssembler, lines ...string) {
	t.Helper()
	for _, l := range lines {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(l), &obj); err != nil {
			t.Fatalf("bad test line: %v", err)
		}
		a.Feed(obj)
	}
}

func TestAssembleContentStream(t *testing.T) {
	a := &ChatStreamAssembler{}
	feed(t, a,
		`{"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
	)
	out, ok := a.Build("m")
	if !ok {
		t.Fatal("assembler saw no choices")
	}
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["content"] != "Hello world" || msg["role"] != "assistant" || msg["finish_reason"] != "stop" {
		t.Errorf("assembled = %s", out)
	}
}

func TestAssembleReasoningStream(t *testing.T) {
	a := &ChatStreamAssembler{}
	feed(t, a,
		`{"choices":[{"delta":{"role":"assistant","reasoning_content":"Let me think"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":" about it."}}]}`,
		`{"choices":[{"delta":{"reasoning":" (alias field)"}}]}`,
		`{"choices":[{"delta":{"content":"Answer."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	out, ok := a.Build("m")
	if !ok {
		t.Fatal("assembler saw no choices")
	}
	var msg struct {
		Content          string `json:"content"`
		ReasoningContent string `json:"reasoning_content"`
	}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.ReasoningContent != "Let me think about it. (alias field)" {
		t.Errorf("reasoning = %q", msg.ReasoningContent)
	}
	if msg.Content != "Answer." {
		t.Errorf("content = %q", msg.Content)
	}
}

func TestAssembleToolCalls(t *testing.T) {
	a := &ChatStreamAssembler{}
	feed(t, a,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Oslo\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	out, ok := a.Build("m")
	if !ok {
		t.Fatal("assembler saw no choices")
	}
	var msg struct {
		Content      string `json:"content"`
		FinishReason string `json:"finish_reason"`
		ToolCalls    []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.FinishReason != "tool_calls" || len(msg.ToolCalls) != 1 {
		t.Fatalf("assembled = %s", out)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Oslo"}` {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestAssembleParallelToolCalls(t *testing.T) {
	a2 := &ChatStreamAssembler{}
	feed(t, a2,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"f0","arguments":"0"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"f1","arguments":"1"}}]}}]}`,
	)
	out, ok := a2.Build("m")
	if !ok {
		t.Fatal("no choices")
	}
	var msg struct {
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	json.Unmarshal(out, &msg)
	if len(msg.ToolCalls) != 2 || msg.ToolCalls[0].ID != "c0" || msg.ToolCalls[1].ID != "c1" {
		t.Errorf("parallel calls = %s", out)
	}
}

func TestAssembleUsageOnlyStream(t *testing.T) {
	a := &ChatStreamAssembler{}
	feed(t, a, `{"choices":[],"usage":{"prompt_tokens":1}}`)
	if _, ok := a.Build("m"); ok {
		t.Error("usage-only stream should not produce a message")
	}
}

func TestResponseObjectFrom(t *testing.T) {
	ev := `{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":9}}}`
	var obj map[string]json.RawMessage
	json.Unmarshal([]byte(ev), &obj)
	ro := ResponseObjectFrom(obj)
	if ro == nil {
		t.Fatal("response object not extracted")
	}
	if string(ro) == "" || !json.Valid(ro) {
		t.Errorf("invalid response object: %s", ro)
	}

	other := `{"type":"response.output_text.delta","delta":"hi"}`
	json.Unmarshal([]byte(other), &obj)
	if ResponseObjectFrom(obj) != nil {
		t.Error("non-completed event yielded a response object")
	}
}
