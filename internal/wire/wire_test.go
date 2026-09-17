package wire

import (
	"encoding/json"
	"testing"
)

func TestExtractUsageChatSchema(t *testing.T) {
	// From the findings page: Hyper's usage, cache hit.
	body := `{"id":"x","model":"glm-5.3-flash","choices":[],
	  "usage":{"prompt_tokens":6819,"completion_tokens":3,"total_tokens":6822,
	           "prompt_tokens_details":{"cached_tokens":6656},
	           "completion_tokens_details":{"reasoning_tokens":3},
	           "cost":{"usd":0.000454}}}`

	u, raw, ok := ExtractUsageFromBytes([]byte(body))
	if !ok {
		t.Fatal("usage not found")
	}
	if u.PromptTokens != 6819 || u.CompletionToken != 3 || u.CachedTokens != 6656 || u.ReasoningTokens != 3 {
		t.Errorf("usage = %+v", u)
	}
	if u.UpstreamCostUSD == nil || *u.UpstreamCostUSD != 0.000454 {
		t.Errorf("upstream cost = %v", u.UpstreamCostUSD)
	}
	if raw == nil {
		t.Error("raw usage not returned")
	}
}

func TestExtractUsageCacheMissOmittedFields(t *testing.T) {
	// On cache miss prompt_tokens_details is absent entirely — cached = 0.
	body := `{"usage":{"prompt_tokens":106,"completion_tokens":2}}`
	u, _, ok := ExtractUsageFromBytes([]byte(body))
	if !ok {
		t.Fatal("usage not found")
	}
	if u.PromptTokens != 106 || u.CachedTokens != 0 {
		t.Errorf("usage = %+v, want cached=0 (omitted means miss)", u)
	}
}

func TestExtractUsageResponsesSchema(t *testing.T) {
	// responses format: input_tokens/output_tokens naming.
	resp := `{"model":"m","usage":{"input_tokens":100,"output_tokens":20,
	          "input_tokens_details":{"cached_tokens":64},
	          "output_tokens_details":{"reasoning_tokens":5}}}`
	u, _, ok := ExtractUsageFromBytes([]byte(resp))
	if !ok {
		t.Fatal("usage not found")
	}
	if u.PromptTokens != 100 || u.CompletionToken != 20 || u.CachedTokens != 64 || u.ReasoningTokens != 5 {
		t.Errorf("usage = %+v", u)
	}
}

func TestExtractUsageResponsesEvent(t *testing.T) {
	// response.completed SSE event: usage nested inside "response".
	ev := `{"type":"response.completed","response":{"model":"m",
	         "usage":{"input_tokens":50,"output_tokens":7}}}`
	u, _, ok := ExtractUsageFromBytes([]byte(ev))
	if !ok {
		t.Fatal("usage not found in response.completed event")
	}
	if u.PromptTokens != 50 || u.CompletionToken != 7 {
		t.Errorf("usage = %+v", u)
	}
}

func TestExtractUsageAbsent(t *testing.T) {
	if _, _, ok := ExtractUsageFromBytes([]byte(`{"choices":[]}`)); ok {
		t.Error("usage reported for object without one")
	}
}

func TestRewriteModel(t *testing.T) {
	in := []byte(`{"model":"alias","messages":[],"other":{"model":"nested-unchanged"}}`)
	out, err := RewriteModel(in, "upstream-id")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	json.Unmarshal(out, &obj)
	if obj["model"] != "upstream-id" {
		t.Errorf("model = %v", obj["model"])
	}
	nested := obj["other"].(map[string]any)
	if nested["model"] != "nested-unchanged" {
		t.Errorf("nested model touched: %v", nested["model"])
	}

	// Body without model passes through unchanged.
	same, err := RewriteModel([]byte(`{"foo":1}`), "x")
	if err != nil || string(same) != `{"foo":1}` {
		t.Errorf("no-model body rewritten: %s", same)
	}
}

func TestParseRequest(t *testing.T) {
	m, stream, err := ParseRequest([]byte(`{"model":"gpt-x","stream":true}`))
	if err != nil || m != "gpt-x" || !stream {
		t.Errorf("m=%q stream=%v err=%v", m, stream, err)
	}
	if _, _, err := ParseRequest([]byte(`{"stream":false}`)); err == nil {
		t.Error("missing model accepted")
	}
	if _, _, err := ParseRequest([]byte(`not-json`)); err == nil {
		t.Error("bad JSON accepted")
	}
}
