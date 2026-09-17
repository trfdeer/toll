package e2e

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trfdeer/toll/internal/store"
)

// readBody drains a request body (stub-side inspection).
func readBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestE2EFullFlow(t *testing.T) {
	stub := newStub(t)

	nonStreamBody := `{"id":"1","model":"glm-5.3-flash","choices":[{"message":{"role":"assistant","content":"ok"}}],
	  "usage":{"prompt_tokens":2000,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":1024}}}`
	stub.mu.Lock()
	stub.chat = func(w http.ResponseWriter, r *http.Request) {
		// The upstream must see its own model ID and the upstream key.
		body := readBody(r)
		if !strings.Contains(body, `"model":"glm-5.3-flash"`) {
			t.Errorf("upstream got model not rewritten: %s", body)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Errorf("upstream key not swapped: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(nonStreamBody))
	}
	stub.mu.Unlock()

	g := startGateway(t, stub, store.KeyFilter{}, store.KeyFilter{})

	// 1. Models: aliased, filtered, metadata intact.
	resp, body := g.get(t, "/v1/models", g.key)
	if resp.StatusCode != 200 {
		t.Fatalf("models status = %d", resp.StatusCode)
	}
	models := parseModels(t, body)
	if len(models) != 1 || models[0]["id"] != "stub/glm-5.3-flash" {
		t.Fatalf("models = %v", models)
	}
	if _, ok := models[0]["pricing"]; !ok {
		t.Error("pricing metadata stripped")
	}

	// 2. Non-streaming chat: alias both ways + usage/cost recorded.
	resp, body = g.post(t, "/v1/chat/completions", g.key, `{"model":"stub/glm-5.3-flash","messages":[]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"model":"stub/glm-5.3-flash"`) {
		t.Fatalf("chat = %d %s", resp.StatusCode, body)
	}
	waitFor(t, "usage event", func() bool {
		evs, _ := g.st.UsageEvents(t.Context())
		return len(evs) == 1
	})
	evs, _ := g.st.UsageEvents(t.Context())
	if evs[0].CachedTokens != 1024 || evs[0].CostUSD == nil {
		t.Errorf("usage event = %+v", evs[0])
	}

	// 3. Streaming chat: alias in chunks, usage captured, reassembled.
	stub.mu.Lock()
	stub.chat = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"model\":\"glm-5.3-flash\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		w.Write([]byte("data: {\"model\":\"glm-5.3-flash\",\"choices\":[{\"delta\":{\"content\":\"y\"}}]}\n\n"))
		w.Write([]byte("data: {\"model\":\"glm-5.3-flash\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":2}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}
	stub.mu.Unlock()

	resp, body = g.post(t, "/v1/chat/completions", g.key, `{"model":"stub/glm-5.3-flash","stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	if strings.Count(body, `"model":"stub/glm-5.3-flash"`) != 3 {
		t.Errorf("alias not in all chunks:\n%s", body)
	}
	if strings.Contains(body, `"model":"glm-5.3-flash"`) {
		t.Errorf("raw upstream ID leaked:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("[DONE] lost")
	}
	waitFor(t, "stream usage event", func() bool {
		evs, _ := g.st.UsageEvents(t.Context())
		return len(evs) == 2
	})

	// Transcript reassembled.
	trs, _ := g.st.Transcripts(t.Context())
	if len(trs) != 2 {
		t.Fatalf("transcripts = %d", len(trs))
	}
	if !strings.Contains(trs[1].ResponseJSON, `"content":"hey"`) {
		t.Errorf("stream transcript not reassembled: %s", trs[1].ResponseJSON)
	}

	// 4. Responses format (non-streaming).
	stub.mu.Lock()
	stub.respons = func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"glm-5.3-flash","usage":{"input_tokens":50,"output_tokens":4}}`))
	}
	stub.mu.Unlock()
	resp, _ = g.post(t, "/v1/responses", g.key, `{"model":"stub/glm-5.3-flash","input":"hi"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("responses status = %d", resp.StatusCode)
	}
	waitFor(t, "responses usage event", func() bool {
		evs, _ := g.st.UsageEvents(t.Context())
		return len(evs) == 3 && evs[2].PromptTokens == 50
	})
}

func TestE2EKeyFilteringAndAuth(t *testing.T) {
	stub := newStub(t)
	// A model-include list without glm means the key cannot see or call it.
	g := startGateway(t, stub, store.KeyFilter{}, store.KeyFilter{
		Mode: "include", Values: []string{"stub/other"},
	})

	// The key cannot see the glm model.
	resp, body := g.get(t, "/v1/models", g.key)
	if resp.StatusCode != 200 || len(parseModels(t, body)) != 0 {
		t.Errorf("filtered models leaked: %s", body)
	}

	// And cannot call it.
	resp, body = g.post(t, "/v1/chat/completions", g.key, `{"model":"stub/glm-5.3-flash"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("disallowed model call = %d %s, want 404", resp.StatusCode, body)
	}

	// Bad / missing key.
	resp, _ = g.get(t, "/v1/models", "gw_bogus")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bogus key = %d", resp.StatusCode)
	}
	resp, _ = g.get(t, "/v1/models", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing key = %d", resp.StatusCode)
	}
}

func TestE2EMalformedSSEPassesThrough(t *testing.T) {
	stub := newStub(t)
	stub.mu.Lock()
	stub.chat = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Garbage interleaved with valid chunks must flow through untouched.
		w.Write([]byte("this is not sse\n\n"))
		w.Write([]byte("data: {broken json\n\n"))
		w.Write([]byte("data: {\"model\":\"glm-5.3-flash\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}
	stub.mu.Unlock()
	g := startGateway(t, stub, store.KeyFilter{}, store.KeyFilter{})

	resp, body := g.post(t, "/v1/chat/completions", g.key, `{"model":"stub/glm-5.3-flash","stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "this is not sse") || !strings.Contains(body, `data: {broken json`) {
		t.Errorf("malformed lines not passed through:\n%s", body)
	}
	if !strings.Contains(body, `"model":"stub/glm-5.3-flash"`) {
		t.Errorf("valid chunk not rewritten:\n%s", body)
	}
}

func TestE2EUpstreamErrorPassesThrough(t *testing.T) {
	stub := newStub(t)
	stub.mu.Lock()
	stub.chat = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInsufficientStorage)
		w.Write([]byte(`{"error":{"message":"storage full","custom":"shape"}}`))
	}
	stub.mu.Unlock()
	g := startGateway(t, stub, store.KeyFilter{}, store.KeyFilter{})

	resp, body := g.post(t, "/v1/chat/completions", g.key, `{"model":"stub/glm-5.3-flash"}`)
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507 verbatim", resp.StatusCode)
	}
	if !strings.Contains(body, `"custom":"shape"`) {
		t.Errorf("error body not verbatim: %s", body)
	}

	// No usage events for failed calls.
	evs, _ := g.st.UsageEvents(t.Context())
	if len(evs) != 0 {
		t.Errorf("usage recorded for failed call: %+v", evs)
	}
}

func TestE2ERefreshRace(t *testing.T) {
	stub := newStub(t)
	stub.mu.Lock()
	stub.models = `{"data":[{"id":"m1"},{"id":"m2"}]}`
	stub.mu.Unlock()
	g := startGateway(t, stub, store.KeyFilter{}, store.KeyFilter{})

	// Hammer model listing + chat while the registry is rewritten under it.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				// Registry churn: flip catalogs like a refresh would.
				stub.mu.Lock()
				if strings.Contains(stub.models, "m3") {
					stub.models = `{"data":[{"id":"m1"},{"id":"m2"}]}`
				} else {
					stub.models = `{"data":[{"id":"m1"},{"id":"m3"}]}`
				}
				stub.mu.Unlock()
				g.post(t, "/v1/chat/completions", g.key, `{"model":"stub/m1"}`)
			}
		}
	}()

	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		resp, _ := g.get(t, "/v1/models", g.key)
		if resp.StatusCode != 200 {
			t.Fatalf("models listing failed under registry churn: %d", resp.StatusCode)
		}
	}
	close(stop)
	wg.Wait()
}
