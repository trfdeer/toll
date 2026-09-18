package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/trfdeer/toll/internal/keys"
	"github.com/trfdeer/toll/internal/store"
)

func setup(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	logger := dbgLogger()
	h := Auth(st, logger, NewModelsHandler(st, logger))
	return st, h
}

func seedModel(t *testing.T, st *store.Store, upstreamID int64, gatewayID, meta string) {
	t.Helper()
	if _, err := st.ReplaceModels(t.Context(), upstreamID, []store.DiscoveredModel{{
		UpstreamModelID: gatewayID + "-upstream",
		GatewayID:       gatewayID,
		DisplayName:     gatewayID,
		Metadata:        []byte(meta),
	}}); err != nil {
		t.Fatal(err)
	}
}

func makeKey(t *testing.T, st *store.Store, name string, provider, model store.KeyFilter) string {
	t.Helper()
	profileID := int64(1) // the seeded All profile
	if constrained(provider) || constrained(model) {
		id, err := st.CreateProfile(t.Context(), name, provider, model)
		if err != nil {
			t.Fatal(err)
		}
		profileID = id
	}
	plaintext, hash, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateVirtualKey(t.Context(), name, hash, profileID); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

// constrained reports whether a filter actually restricts anything.
func constrained(f store.KeyFilter) bool {
	return f.Mode != "" && f.Mode != "none" && len(f.Values) > 0
}

func TestModelsRequiresAuth(t *testing.T) {
	_, h := setup(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw_bogus")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestModelsMergedAndFiltered(t *testing.T) {
	st, h := setup(t)

	// Two upstreams, each with one model.
	up1, _ := st.UpsertUpstream(t.Context(), "hyper", "https://a/v1", "k", 300, 0)
	up2, _ := st.UpsertUpstream(t.Context(), "other", "https://b/v1", "k", 300, 1)
	seedModel(t, st, up1, "hyper/glm", `{"id":"glm","pricing":{"input":0.16},"context_window":202000}`)
	seedModel(t, st, up2, "other/m", `{"id":"m","pricing":{"input":1.0}}`)

	openKey := makeKey(t, st, "open", store.KeyFilter{}, store.KeyFilter{})
	hyperKey := makeKey(t, st, "hyper-only",
		store.KeyFilter{Mode: "include", Values: []string{"hyper"}}, store.KeyFilter{})

	get := func(key string) (int, []map[string]any, string) {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, body.Data, rec.Body.String()
	}

	code, data, body := get(openKey)
	if code != 200 || len(data) != 2 {
		t.Fatalf("open key: status=%d models=%d body=%s", code, len(data), body)
	}

	code, data, _ = get(hyperKey)
	if code != 200 || len(data) != 1 {
		t.Fatalf("hyper-only key: status=%d models=%d, want 200/1", code, len(data))
	}
	if data[0]["id"] != "hyper/glm" {
		t.Errorf("filtered model id = %v, want hyper/glm (rewritten to gateway ID)", data[0]["id"])
	}
	// Metadata fields preserved through re-emit.
	if data[0]["context_window"] != float64(202000) {
		t.Errorf("context_window = %v, want 202000", data[0]["context_window"])
	}
}

// TestModelsUnifiedShapeAndProviderSource checks that every entry is emitted
// in the gateway's one model shape and that the provider filter keys off the
// discovery source, not the ID prefix.
func TestModelsUnifiedShapeAndProviderSource(t *testing.T) {
	st, h := setup(t)

	up, _ := st.UpsertUpstream(t.Context(), "zeph", "https://z/v1", "k", 300, 0)
	// Gateway ID carries a "llamacpp" prefix, but the model came from "zeph".
	seedModel(t, st, up, "llamacpp/gemma",
		`{"id":"llamacpp/gemma","max_model_len":256000,"capabilities":{"vision":false},"pricing":{"input":0.1}}`)

	get := func(key string) []map[string]any {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Data
	}

	// Key restricted to the discovery source "zeph" sees the model.
	data := get(makeKey(t, st, "by-source", store.KeyFilter{Mode: "include", Values: []string{"zeph"}}, store.KeyFilter{}))
	if len(data) != 1 {
		t.Fatalf("by-source key: models = %d, want 1", len(data))
	}
	m := data[0]
	if m["id"] != "llamacpp/gemma" {
		t.Errorf("id = %v", m["id"])
	}
	if m["owned_by"] != "zeph" {
		t.Errorf("owned_by = %v, want the discovery source zeph", m["owned_by"])
	}
	if m["object"] != "model" {
		t.Errorf("object = %v, want model", m["object"])
	}
	if _, ok := m["created"]; !ok {
		t.Error("created missing from unified shape")
	}
	// max_model_len is normalized into the unified context_window field.
	if m["context_window"] != float64(256000) {
		t.Errorf("context_window = %v, want 256000", m["context_window"])
	}
	if _, ok := m["capabilities"]; !ok {
		t.Error("capabilities dropped from unified shape")
	}

	// A key restricted to the ID prefix "llamacpp" must NOT see it — proving
	// the provider decision is not derived from the name.
	if data := get(makeKey(t, st, "by-prefix", store.KeyFilter{Mode: "include", Values: []string{"llamacpp"}}, store.KeyFilter{})); len(data) != 0 {
		t.Errorf("by-prefix key saw %d models, want 0", len(data))
	}
}

// TestProviderStatusHidesModels checks that a provider which is unreachable
// or disabled hides all of its models from /v1/models and refuses routing,
// without removing them from the registry.
func TestProviderStatusHidesModels(t *testing.T) {
	st, h := setup(t)
	up, _ := st.UpsertUpstream(t.Context(), "zeph", "https://z/v1", "k", 300, 0)
	seedModel(t, st, up, "zeph/glm", `{"id":"glm"}`)
	key := makeKey(t, st, "open", store.KeyFilter{}, store.KeyFilter{})

	list := func() int {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return len(body.Data)
	}

	if n := list(); n != 1 {
		t.Fatalf("reachable provider: models = %d, want 1", n)
	}

	// Unreachable → hidden and unroutable.
	if err := st.SetUpstreamReachable(t.Context(), "zeph", false, "connection refused"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RouteForModel(t.Context(), "zeph/glm"); err == nil {
		t.Error("unreachable provider was routable")
	}
	if n := list(); n != 0 {
		t.Errorf("unreachable provider: models = %d, want 0", n)
	}

	// Recovered → visible again.
	if err := st.SetUpstreamReachable(t.Context(), "zeph", true, ""); err != nil {
		t.Fatal(err)
	}
	if n := list(); n != 1 {
		t.Errorf("recovered provider: models = %d, want 1", n)
	}

	// Disabled → hidden and unroutable, independently of reachability.
	if err := st.SetUpstreamDisabled(t.Context(), "zeph", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RouteForModel(t.Context(), "zeph/glm"); err == nil {
		t.Error("disabled provider was routable")
	}
	if n := list(); n != 0 {
		t.Errorf("disabled provider: models = %d, want 0", n)
	}
}

func TestDisabledModelHidden(t *testing.T) {
	st, h := setup(t)

	up, _ := st.UpsertUpstream(t.Context(), "hyper", "https://a/v1", "k", 300, 0)
	if _, err := st.ReplaceModels(t.Context(), up, []store.DiscoveredModel{
		{UpstreamModelID: "on-upstream", GatewayID: "hyper/on", DisplayName: "on", Metadata: []byte(`{"id":"on"}`)},
		{UpstreamModelID: "off-upstream", GatewayID: "hyper/off", DisplayName: "off", Metadata: []byte(`{"id":"off"}`), Disabled: boolPtr(true)},
	}); err != nil {
		t.Fatal(err)
	}

	key := makeKey(t, st, "open", store.KeyFilter{}, store.KeyFilter{})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || body.Data[0]["id"] != "hyper/on" {
		t.Fatalf("models = %v, want only hyper/on (disabled hidden)", body.Data)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestPausedKeyRejected(t *testing.T) {
	st, h := setup(t)
	plaintext := makeKey(t, st, "paused", store.KeyFilter{}, store.KeyFilter{})
	if err := st.PauseVirtualKey(t.Context(), "paused"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for paused key", rec.Code)
	}
}

func TestRevokedKeyRejected(t *testing.T) {
	st, h := setup(t)
	plaintext := makeKey(t, st, "doomed", store.KeyFilter{}, store.KeyFilter{})
	if err := st.RevokeVirtualKey(t.Context(), "doomed"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for revoked key", rec.Code)
	}
}
