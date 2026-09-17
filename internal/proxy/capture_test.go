package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/log"

	"github.com/trfdeer/toll/internal/api"
	"github.com/trfdeer/toll/internal/keys"
	"github.com/trfdeer/toll/internal/store"
)

func captureSetup(t *testing.T) (*store.Store, http.Handler, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	plaintext, hash, _ := keys.Generate()
	if _, err := st.CreateVirtualKey(t.Context(), "test", hash, store.KeyFilter{}, store.KeyFilter{}); err != nil {
		t.Fatal(err)
	}

	logger := log.NewWithOptions(nil, log.Options{Level: log.ErrorLevel})
	// Real auth middleware, so usage events get a valid key_id.
	handler := api.Auth(st, logger, NewCapture(st, logger))
	return st, handler, plaintext
}

func TestCaptureNonStreaming(t *testing.T) {
	st, handler, key := captureSetup(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-k" {
			t.Errorf("auth = %q", got)
		}
		if !strings.Contains(readAllBody(r), `"model":"upstream-glm"`) {
			t.Errorf("upstream body not rewritten: %s", readAllBody(r))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","model":"upstream-glm","choices":[],
		  "usage":{"prompt_tokens":100,"completion_tokens":10,
		           "prompt_tokens_details":{"cached_tokens":64}}}`))
	}))
	defer upstream.Close()

	id, err := st.UpsertUpstream(t.Context(), "u", upstream.URL+"/v1", "upstream-k", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceModels(t.Context(), id, []store.DiscoveredModel{{
		UpstreamModelID: "upstream-glm", GatewayID: "glm", DisplayName: "glm",
		Metadata: []byte(`{"pricing":{"input":1.0,"output":2.0,"cache_hit":0.5}}`),
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"model":"glm"`) {
		t.Errorf("response model not rewritten back to alias: %s", rec.Body.String())
	}

	events, err := st.UsageEvents(t.Context())
	if err != nil || len(events) != 1 {
		t.Fatalf("usage events = %d err=%v, want 1", len(events), err)
	}
	e := events[0]
	if e.PromptTokens != 100 || e.CompletionToken != 10 || e.CachedTokens != 64 {
		t.Errorf("usage = %+v", e)
	}
	// Cost from seeded pricing: (36*1.0 + 64*0.5 + 10*2.0)/1e6 = 88e-6.
	if e.CostUSD == nil || math.Abs(*e.CostUSD-88e-6) > 1e-12 {
		t.Errorf("cost = %v, want 88e-6", e.CostUSD)
	}
	if e.GatewayModel != "glm" || e.UpstreamModel != "upstream-glm" {
		t.Errorf("models = %q/%q", e.GatewayModel, e.UpstreamModel)
	}
}

func TestTranscriptNonStreaming(t *testing.T) {
	st, handler, key := captureSetup(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"model":"upstream-glm","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	seedModel(t, st, upstream.URL, "glm", "upstream-glm", `{}`)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	trs, err := st.Transcripts(t.Context())
	if err != nil || len(trs) != 1 {
		t.Fatalf("transcripts = %d err=%v, want 1", len(trs), err)
	}
	tr := trs[0]
	if tr.RequestJSON == "" || !strings.Contains(tr.RequestJSON, `"hello"`) {
		t.Errorf("request not stored verbatim: %s", tr.RequestJSON)
	}
	if !strings.Contains(tr.ResponseJSON, `"content":"hi"`) {
		t.Errorf("response not stored: %s", tr.ResponseJSON)
	}
	if tr.Status != 200 || tr.CompletedAt == "" {
		t.Errorf("status=%d completed=%q", tr.Status, tr.CompletedAt)
	}
}

func TestTranscriptStreamingReassembled(t *testing.T) {
	st, handler, key := captureSetup(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, line := range []string{
			`data: {"model":"upstream-glm","choices":[{"delta":{"role":"assistant","content":"par"}}]}`,
			`data: {"model":"upstream-glm","choices":[{"delta":{"content":"tial"}}]}`,
			`data: {"model":"upstream-glm","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`,
			`data: {"model":"upstream-glm","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
			`data: {"model":"upstream-glm","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: {"model":"upstream-glm","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
			`data: [DONE]`,
		} {
			w.Write([]byte(line + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	seedModel(t, st, upstream.URL, "glm", "upstream-glm", `{}`)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	trs, err := st.Transcripts(t.Context())
	if err != nil || len(trs) != 1 {
		t.Fatalf("transcripts = %d err=%v, want 1", len(trs), err)
	}
	tr := trs[0]

	var msg struct {
		Content      string `json:"content"`
		FinishReason string `json:"finish_reason"`
		ToolCalls    []struct {
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(tr.ResponseJSON), &msg); err != nil {
		t.Fatalf("response_json not valid assembled message: %v (%s)", err, tr.ResponseJSON)
	}
	if msg.Content != "partial" || msg.FinishReason != "tool_calls" {
		t.Errorf("assembled = %s", tr.ResponseJSON)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Errorf("tool call not reassembled: %s", tr.ResponseJSON)
	}
	if tr.PromptTokens != 7 || tr.CompletionToken != 3 {
		t.Errorf("tokens = %d/%d", tr.PromptTokens, tr.CompletionToken)
	}
	if tr.ConversationID == "" {
		t.Error("no conversation grouping")
	}
}

func TestTranscriptStreamingReasoning(t *testing.T) {
	st, handler, key := captureSetup(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, line := range []string{
			`data: {"model":"upstream-glm","choices":[{"delta":{"role":"assistant","reasoning_content":"thinking…"}}]}`,
			`data: {"model":"upstream-glm","choices":[{"delta":{"content":"done"}}]}`,
			`data: {"model":"upstream-glm","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		} {
			w.Write([]byte(line + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	seedModel(t, st, upstream.URL, "glm", "upstream-glm", `{}`)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	trs, err := st.Transcripts(t.Context())
	if err != nil || len(trs) != 1 {
		t.Fatalf("transcripts = %d err=%v, want 1", len(trs), err)
	}
	if !strings.Contains(trs[0].ResponseJSON, `"reasoning_content":"thinking…"`) {
		t.Errorf("reasoning not captured in transcript: %s", trs[0].ResponseJSON)
	}
}

func TestConversationHeaderGroups(t *testing.T) {
	st, handler, key := captureSetup(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"model":"upstream-glm","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	seedModel(t, st, upstream.URL, "glm", "upstream-glm", `{}`)

	for _, body := range []string{
		`{"model":"glm","messages":[{"role":"user","content":"turn 1"}]}`,
		`{"model":"glm","messages":[{"role":"user","content":"turn 2"}]}`,
	} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Toll-Conversation-Id", "chat-42")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status = %d", rec.Code)
		}
	}

	trs, _ := st.Transcripts(t.Context())
	if len(trs) != 2 {
		t.Fatalf("transcripts = %d, want 2", len(trs))
	}
	if trs[0].ConversationID != trs[1].ConversationID {
		t.Errorf("header did not group: %q vs %q", trs[0].ConversationID, trs[1].ConversationID)
	}

	// Without the header, distinct bodies get distinct conversations.
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm","messages":[{"role":"user","content":"turn 3"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	trs, _ = st.Transcripts(t.Context())
	if len(trs) != 3 || trs[2].ConversationID == trs[0].ConversationID {
		t.Errorf("distinct request shared a conversation: %s", trs[2].ConversationID)
	}
}

// seedModel registers one model for capture tests.
func seedModel(t *testing.T, st *store.Store, baseURL, gatewayID, upstreamID, meta string) {
	t.Helper()
	id, err := st.UpsertUpstream(t.Context(), "u", baseURL+"/v1", "upstream-k", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceModels(t.Context(), id, []store.DiscoveredModel{{
		UpstreamModelID: upstreamID, GatewayID: gatewayID, DisplayName: gatewayID,
		Metadata: []byte(meta),
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureStreaming(t *testing.T) {
	st, handler, key := captureSetup(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range []string{
			`data: {"model":"upstream-glm","choices":[{"delta":{"content":"hi"}}]}`,
			``,
			`data: {"model":"upstream-glm","choices":[],"usage":{"prompt_tokens":500,"completion_tokens":9}}`,
			``,
			`data: [DONE]`,
			``,
		} {
			w.Write([]byte(line + "\n"))
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	id, _ := st.UpsertUpstream(t.Context(), "u", upstream.URL+"/v1", "k", 0, 0)
	if _, err := st.ReplaceModels(t.Context(), id, []store.DiscoveredModel{{
		UpstreamModelID: "upstream-glm", GatewayID: "glm", DisplayName: "glm", Metadata: []byte(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"model":"glm"`) {
		t.Errorf("chunk model not rewritten to alias: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("[DONE] lost: %s", body)
	}
	if strings.Contains(body, `"model":"upstream-glm"`) {
		t.Errorf("upstream ID leaked to client: %s", body)
	}

	events, _ := st.UsageEvents(t.Context())
	if len(events) != 1 || events[0].PromptTokens != 500 || events[0].CompletionToken != 9 {
		t.Fatalf("usage events = %+v", events)
	}
}

func TestCaptureUnknownModel(t *testing.T) {
	_, handler, key := captureSetup(t)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"nope","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "nope-upstream") {
		t.Error("internal details leaked")
	}
}

func TestCaptureUpstreamErrorPassesThrough(t *testing.T) {
	st, handler, key := captureSetup(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte(`{"weird":"upstream-error-shape"}`))
	}))
	defer upstream.Close()

	id, _ := st.UpsertUpstream(t.Context(), "u", upstream.URL+"/v1", "k", 0, 0)
	if _, err := st.ReplaceModels(t.Context(), id, []store.DiscoveredModel{{
		UpstreamModelID: "m", GatewayID: "m", DisplayName: "m", Metadata: []byte(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot || !strings.Contains(rec.Body.String(), "upstream-error-shape") {
		t.Fatalf("status=%d body=%s — errors must pass verbatim", rec.Code, rec.Body.String())
	}
}

func readAllBody(r *http.Request) string {
	b := make([]byte, 4096)
	n, _ := r.Body.Read(b)
	return string(b[:n])
}
