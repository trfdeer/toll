package admin

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"gopkg.in/yaml.v3"

	"github.com/trfdeer/toll/internal/config"
	"github.com/trfdeer/toll/internal/store"
)

func setup(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logger := log.NewWithOptions(nil, log.Options{Level: log.ErrorLevel})
	return st, Handler(st, logger)
}

func TestProvidersAndUsage(t *testing.T) {
	st, h := setup(t)

	upID, _ := st.UpsertUpstream(t.Context(), "hyper", "https://x/v1", "k", 300, 0)
	st.ReplaceModels(t.Context(), upID, []store.DiscoveredModel{{
		UpstreamModelID: "glm", GatewayID: "hyper/glm", DisplayName: "glm", Metadata: []byte(`{}`),
	}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/providers", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var ups []struct {
		Name       string `json:"name"`
		BaseURL    string `json:"baseURL"`
		ModelCount int    `json:"modelCount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ups); err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].Name != "hyper" || ups[0].BaseURL != "https://x/v1" || ups[0].ModelCount != 1 {
		t.Errorf("unexpected providers: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var models []struct {
		GatewayID string `json:"gatewayId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].GatewayID != "hyper/glm" {
		t.Errorf("unexpected models: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var usage struct {
		Rows      []json.RawMessage `json:"rows"`
		TotalReqs int               `json:"totalReqs"`
		TotalCost string            `json:"totalCost"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.TotalReqs != 0 || !strings.Contains(usage.TotalCost, "USD") {
		t.Errorf("unexpected usage: %s", rec.Body.String())
	}

	// Requests list (empty).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests", nil))
	var reqs struct {
		Requests []json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reqs); err != nil {
		t.Fatal(err)
	}
	if len(reqs.Requests) != 0 {
		t.Errorf("unexpected requests: %s", rec.Body.String())
	}
}

func TestUsageAndRequestFilters(t *testing.T) {
	st, h := setup(t)

	upID, _ := st.UpsertUpstream(t.Context(), "hyper", "https://x/v1", "k", 300, 0)
	keyID, _ := st.CreateVirtualKey(t.Context(), "app", "hash", 1)
	st.RecordUsage(t.Context(), store.UsageEvent{
		KeyID: keyID, UpstreamID: upID, GatewayModel: "m", UpstreamModel: "m",
		PromptTokens: 1, CompletionToken: 1,
	})
	st.EnsureConversation(t.Context(), "conv", keyID)
	tid, _ := st.CreateTranscript(t.Context(), "conv", "m", "m-upstream", `{"model":"m"}`)
	st.CompleteTranscript(t.Context(), tid, `{"ok":true}`, 200, store.UsageEvent{KeyID: keyID, UpstreamID: upID})

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	var reqs struct {
		Requests []struct {
			DurationMS *int64 `json:"durationMs"`
		} `json:"requests"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(get("/api/requests?key=app").Body.Bytes(), &reqs); err != nil {
		t.Fatal(err)
	}
	if len(reqs.Requests) != 1 || reqs.Total != 1 {
		t.Errorf("key=app requests = %d/%d, want 1/1", len(reqs.Requests), reqs.Total)
	}
	if len(reqs.Requests) == 1 && reqs.Requests[0].DurationMS == nil {
		t.Error("completed request is missing durationMs")
	}
	if err := json.Unmarshal(get("/api/requests?key=nope").Body.Bytes(), &reqs); err != nil {
		t.Fatal(err)
	}
	if len(reqs.Requests) != 0 {
		t.Errorf("key=nope requests = %d, want 0", len(reqs.Requests))
	}

	// A lower bound in the future excludes everything.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := json.Unmarshal(get("/api/requests?from="+future).Body.Bytes(), &reqs); err != nil {
		t.Fatal(err)
	}
	if len(reqs.Requests) != 0 {
		t.Errorf("future from requests = %d, want 0", len(reqs.Requests))
	}

	var usage struct {
		TotalReqs int `json:"totalReqs"`
	}
	if err := json.Unmarshal(get("/api/usage?from="+future).Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.TotalReqs != 0 {
		t.Errorf("future from usage totalReqs = %d, want 0", usage.TotalReqs)
	}

	if rec := get("/api/requests?from=not-a-time"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad from status = %d, want 400", rec.Code)
	}
	if rec := get("/api/requests?limit=0"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad limit status = %d, want 400", rec.Code)
	}
}

func TestRequestDetailBuildsConversation(t *testing.T) {
	st, h := setup(t)

	keyID, _ := st.CreateVirtualKey(t.Context(), "app", "hash", 1)
	st.EnsureConversation(t.Context(), "conv", keyID)
	reqBody := `{"model":"m","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`
	id, _ := st.CreateTranscript(t.Context(), "conv", "m", "m-upstream", reqBody)
	respBody := `{"choices":[{"message":{"role":"assistant","content":"hello!"}}]}`
	st.CompleteTranscript(t.Context(), id, respBody, 200, store.UsageEvent{KeyID: keyID, UpstreamID: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests/"+strconv.FormatInt(id, 10), nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got requestDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %s", len(got.Messages), rec.Body.String())
	}
	want := []struct{ role, content string }{
		{"system", "be nice"},
		{"user", "hi"},
		{"assistant", "hello!"},
	}
	for i, w := range want {
		if got.Messages[i].Role != w.role || got.Messages[i].Content != w.content {
			t.Errorf("message %d = %+v, want %s/%q", i, got.Messages[i], w.role, w.content)
		}
	}
	if got.DurationMS == nil {
		t.Error("durationMs is nil for a completed request")
	}

	// Unknown id and non-integer id.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests/9999", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests/nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad id status = %d, want 400", rec.Code)
	}
}

func TestKeysCreateListRevoke(t *testing.T) {
	st, h := setup(t)

	if _, err := st.CreateProfile(t.Context(), "restricted",
		store.KeyFilter{Mode: "include", Values: []string{"hyper"}},
		store.KeyFilter{Mode: "exclude", Values: []string{"hyper/glm"}}, nil); err != nil {
		t.Fatal(err)
	}

	// Create referencing the profile.
	body := `{"name": "my-app", "profile": "restricted"}`
	req := httptest.NewRequest("POST", "/api/keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Plaintext string `json:"plaintext"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Plaintext, "sk-tl-") {
		t.Errorf("plaintext missing: %s", rec.Body.String())
	}

	// List.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/keys", nil))
	var listed struct {
		Keys []keyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Keys) != 1 || listed.Keys[0].Name != "my-app" || listed.Keys[0].Revoked {
		t.Errorf("unexpected keys: %s", rec.Body.String())
	}
	if k := listed.Keys[0]; k.Profile != "restricted" {
		t.Errorf("profile not stored: %s", rec.Body.String())
	}

	// Unknown profile → 422.
	bad := `{"name": "bad", "profile": "nope"}`
	req = httptest.NewRequest("POST", "/api/keys", strings.NewReader(bad))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown profile status = %d, want 422", rec.Code)
	}

	// Duplicate name → 422.
	req = httptest.NewRequest("POST", "/api/keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate status = %d, want 422", rec.Code)
	}

	// Revoke.
	req = httptest.NewRequest("POST", "/api/keys/my-app/revoke", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/keys", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if !listed.Keys[0].Revoked {
		t.Errorf("key not revoked: %s", rec.Body.String())
	}
}

func TestKeysUpdate(t *testing.T) {
	st, h := setup(t)
	if _, err := st.CreateVirtualKey(t.Context(), "old", "hash-a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateVirtualKey(t.Context(), "other", "hash-b", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProfile(t.Context(), "zeph-only",
		store.KeyFilter{Mode: "include", Values: []string{"zeph"}}, store.KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}

	// Rename plus a new profile.
	body := `{"name":"renamed","profile":"zeph-only"}`
	req := httptest.NewRequest("PUT", "/api/keys/old", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update status = %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/keys", nil))
	var listed struct {
		Keys []keyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var found *keyView
	for i := range listed.Keys {
		if listed.Keys[i].Name == "renamed" {
			found = &listed.Keys[i]
		}
	}
	if found == nil {
		t.Fatalf("rename did not take: %s", rec.Body.String())
	}
	if found.Profile != "zeph-only" {
		t.Errorf("profile not updated: %+v", found)
	}

	// Unknown key → 404.
	req = httptest.NewRequest("PUT", "/api/keys/missing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing key status = %d, want 404", rec.Code)
	}

	// Rename onto an existing name → 422.
	dup := `{"name":"other","profile":"All"}`
	req = httptest.NewRequest("PUT", "/api/keys/renamed", strings.NewReader(dup))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate rename status = %d, want 422", rec.Code)
	}

	// Empty name → 422.
	empty := `{"name":"  ","profile":"All"}`
	req = httptest.NewRequest("PUT", "/api/keys/renamed", strings.NewReader(empty))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty name status = %d, want 422", rec.Code)
	}
}

func TestProfilesCRUD(t *testing.T) {
	st, h := setup(t)

	type profileList struct {
		Profiles []profileView `json:"profiles"`
	}
	list := func() profileList {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/profiles", nil))
		var out profileList
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	do := func(method, path, body string) int {
		t.Helper()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// The seeded All profile is present, default and read-only.
	got := list()
	if len(got.Profiles) != 1 || got.Profiles[0].Name != "All" || !got.Profiles[0].IsDefault {
		t.Fatalf("seeded profiles = %+v", got.Profiles)
	}

	// Create.
	if code := do("POST", "/api/profiles",
		`{"name":"zeph","providerFilter":{"mode":"include","values":["zeph"]},"modelFilter":{"mode":"none","values":[]}}`); code != http.StatusNoContent {
		t.Fatalf("create status = %d, want 204", code)
	}

	// Duplicate name → 422.
	if code := do("POST", "/api/profiles",
		`{"name":"zeph","providerFilter":{"mode":"none","values":[]},"modelFilter":{"mode":"none","values":[]}}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate status = %d, want 422", code)
	}

	// Empty include values → 422.
	if code := do("POST", "/api/profiles",
		`{"name":"bad","providerFilter":{"mode":"include","values":[]}}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty include status = %d, want 422", code)
	}

	// The All profile cannot be edited or deleted.
	if code := do("PUT", "/api/profiles/All",
		`{"name":"All","providerFilter":{"mode":"none","values":[]},"modelFilter":{"mode":"none","values":[]}}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("update All status = %d, want 422", code)
	}
	if code := do("DELETE", "/api/profiles/All", ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("delete All status = %d, want 422", code)
	}

	// Rename.
	if code := do("PUT", "/api/profiles/zeph",
		`{"name":"zeph-renamed","providerFilter":{"mode":"include","values":["zeph"]},"modelFilter":{"mode":"none","values":[]}}`); code != http.StatusNoContent {
		t.Fatalf("rename status = %d, want 204", code)
	}

	// A profile in use cannot be deleted.
	if _, err := st.CreateVirtualKey(t.Context(), "app", "hash", 2); err != nil {
		t.Fatal(err)
	}
	if code := do("DELETE", "/api/profiles/zeph-renamed", ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("in-use delete status = %d, want 422", code)
	}

	// Delete once unused.
	if err := st.DeleteVirtualKey(t.Context(), "app"); err != nil {
		t.Fatal(err)
	}
	if code := do("DELETE", "/api/profiles/zeph-renamed", ""); code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", code)
	}
	if code := do("DELETE", "/api/profiles/zeph-renamed", ""); code != http.StatusNotFound {
		t.Fatalf("re-delete status = %d, want 404", code)
	}
}

func TestProfilesDerived(t *testing.T) {
	st, h := setup(t)

	if _, err := st.CreateProfile(t.Context(), "base",
		store.KeyFilter{Mode: "include", Values: []string{"hyper"}},
		store.KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}

	do := func(method, path, body string) int {
		t.Helper()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// A derived profile names its parents and carries no filters.
	if code := do("POST", "/api/profiles", `{"name":"child","parents":["base"]}`); code != http.StatusNoContent {
		t.Fatalf("create derived status = %d, want 204", code)
	}

	listProfiles := func() map[string]profileView {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/profiles", nil))
		var listed struct {
			Profiles []profileView `json:"profiles"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
			t.Fatal(err)
		}
		byName := map[string]profileView{}
		for _, p := range listed.Profiles {
			byName[p.Name] = p
		}
		return byName
	}

	byName := listProfiles()
	if got := byName["child"]; len(got.Parents) != 1 || got.Parents[0] != "base" {
		t.Errorf("derived profile = %+v", got)
	}
	if got := byName["base"]; got.ChildCount != 1 {
		t.Errorf("base childCount = %d, want 1", got.ChildCount)
	}

	// Parent names are trimmed before the store resolves them.
	if code := do("POST", "/api/profiles", `{"name":"padded","parents":[" base "]}`); code != http.StatusNoContent {
		t.Fatalf("padded parent status = %d, want 204", code)
	}
	if got := listProfiles()["padded"]; len(got.Parents) != 1 || got.Parents[0] != "base" {
		t.Errorf("padded parents = %v, want [base]", got.Parents)
	}

	// Renaming onto an existing name is a rejection, not a store 500.
	if code := do("POST", "/api/profiles",
		`{"name":"other","providerFilter":{"mode":"include","values":["z"]}}`); code != http.StatusNoContent {
		t.Fatalf("create other status = %d, want 204", code)
	}
	if code := do("PUT", "/api/profiles/child", `{"name":"other","parents":["base"]}`); code != http.StatusUnprocessableEntity {
		t.Errorf("rename collision status = %d, want 422", code)
	}

	// Filters mixed with parents, unknown parents and self-parenting are 422.
	if code := do("POST", "/api/profiles",
		`{"name":"mix","providerFilter":{"mode":"include","values":["x"]},"parents":["base"]}`); code != http.StatusUnprocessableEntity {
		t.Errorf("mixed status = %d, want 422", code)
	}
	if code := do("POST", "/api/profiles", `{"name":"orphan","parents":["nope"]}`); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown parent status = %d, want 422", code)
	}
	if code := do("PUT", "/api/profiles/base", `{"name":"base","parents":["base"]}`); code != http.StatusUnprocessableEntity {
		t.Errorf("self parent status = %d, want 422", code)
	}
	// Cycle: base inherits child, which already inherits base.
	if code := do("PUT", "/api/profiles/base", `{"name":"base","parents":["child"]}`); code != http.StatusUnprocessableEntity {
		t.Errorf("cycle status = %d, want 422", code)
	}

	// base is used as a parent, so it cannot be deleted yet.
	if code := do("DELETE", "/api/profiles/base", ""); code != http.StatusUnprocessableEntity {
		t.Errorf("delete parent status = %d, want 422", code)
	}
	for _, name := range []string{"child", "padded", "other"} {
		if code := do("DELETE", "/api/profiles/"+name, ""); code != http.StatusNoContent {
			t.Fatalf("delete %s status = %d, want 204", name, code)
		}
	}
	if code := do("DELETE", "/api/profiles/base", ""); code != http.StatusNoContent {
		t.Errorf("delete base after children status = %d, want 204", code)
	}
}

func TestKeysPauseResumeDelete(t *testing.T) {
	_, h := setup(t)

	body := `{"name": "app", "allow": ["*"]}`
	req := httptest.NewRequest("POST", "/api/keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)

	list := func() []keyView {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/keys", nil))
		var out struct {
			Keys []keyView `json:"keys"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Keys
	}

	post := func(path string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		return rec.Code
	}

	if code := post("/api/keys/app/pause"); code != http.StatusNoContent {
		t.Fatalf("pause status = %d", code)
	}
	if ks := list(); len(ks) != 1 || !ks[0].Paused || ks[0].Revoked {
		t.Fatalf("key not paused: %+v", ks)
	}
	if code := post("/api/keys/app/resume"); code != http.StatusNoContent {
		t.Fatalf("resume status = %d", code)
	}
	if ks := list(); len(ks) != 1 || ks[0].Paused {
		t.Fatalf("key still paused: %+v", ks)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/keys/app", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	if ks := list(); len(ks) != 0 {
		t.Fatalf("key not deleted: %+v", ks)
	}

	// Deleting again reports not found.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/keys/app", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", rec.Code)
	}
}

func TestConfigExport(t *testing.T) {
	st, h := setup(t)

	upID, _ := st.UpsertUpstream(t.Context(), "hyper", "https://hyper.charm.land/v1", "secret", 300, 0)
	st.ReplaceModels(t.Context(), upID, []store.DiscoveredModel{
		{UpstreamModelID: "glm", GatewayID: "hyper/glm", DisplayName: "Hyper GLM",
			Metadata: []byte(`{"id":"glm","context_window":128000,"tier":"standard"}`)},
		{UpstreamModelID: "qwen", GatewayID: "qwen", DisplayName: "Qwen",
			Metadata: []byte(`{"id":"qwen"}`)},
	})
	if _, err := st.CreateProfile(t.Context(), "glm-only",
		store.KeyFilter{Mode: "include", Values: []string{"hyper"}},
		store.KeyFilter{Mode: "exclude", Values: []string{"hyper/hidden"}}, nil); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/config", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Errorf("content-type = %q, want yaml", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "toll.yaml") {
		t.Errorf("content-disposition = %q", cd)
	}

	var got struct {
		Profiles []struct {
			Name           string `yaml:"name"`
			ProviderFilter struct {
				Mode   string   `yaml:"mode"`
				Values []string `yaml:"values"`
			} `yaml:"provider_filter"`
			ModelFilter struct {
				Mode   string   `yaml:"mode"`
				Values []string `yaml:"values"`
			} `yaml:"model_filter"`
		} `yaml:"profiles"`
		Upstreams []struct {
			Name      string `yaml:"name"`
			URL       string `yaml:"url"`
			APIKeyEnv string `yaml:"api_key_env"`
			Refresh   string `yaml:"refresh_interval"`
			Models    []struct {
				ID       string         `yaml:"id"`
				Alias    string         `yaml:"alias"`
				Name     string         `yaml:"name"`
				Metadata map[string]any `yaml:"metadata"`
			} `yaml:"models"`
		} `yaml:"upstreams"`
	}
	if err := yaml.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("export is not valid YAML: %v\n%s", err, rec.Body.String())
	}
	if len(got.Upstreams) != 1 {
		t.Fatalf("upstreams = %d, want 1\n%s", len(got.Upstreams), rec.Body.String())
	}
	u := got.Upstreams[0]
	if u.Name != "hyper" || u.URL != "https://hyper.charm.land/v1" || u.APIKeyEnv != "HYPER_API_KEY" {
		t.Errorf("unexpected upstream: %+v", u)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("export leaked the api key: %s", rec.Body.String())
	}
	if len(u.Models) != 2 {
		t.Fatalf("models = %d, want 2\n%s", len(u.Models), rec.Body.String())
	}
	// Ordered by gateway id: hyper/glm then qwen.
	first := u.Models[0]
	if first.ID != "glm" || first.Alias != "hyper/glm" || first.Name != "Hyper GLM" {
		t.Errorf("unexpected first model: %+v", first)
	}
	if _, ok := first.Metadata["id"]; ok {
		t.Errorf("metadata id should be stripped: %+v", first.Metadata)
	}
	if first.Metadata["context_window"] != 128000 || first.Metadata["tier"] != "standard" {
		t.Errorf("metadata not preserved: %+v", first.Metadata)
	}
	if second := u.Models[1]; second.ID != "qwen" || second.Alias != "" {
		t.Errorf("alias should be omitted when identical to upstream id: %+v", second)
	}

	if len(got.Profiles) != 1 {
		t.Fatalf("profiles = %d, want 1\n%s", len(got.Profiles), rec.Body.String())
	}
	p := got.Profiles[0]
	if p.Name != "glm-only" ||
		p.ProviderFilter.Mode != "include" || len(p.ProviderFilter.Values) != 1 || p.ProviderFilter.Values[0] != "hyper" ||
		p.ModelFilter.Mode != "exclude" || len(p.ModelFilter.Values) != 1 || p.ModelFilter.Values[0] != "hyper/hidden" {
		t.Errorf("unexpected exported profile: %+v", p)
	}
	if strings.Contains(rec.Body.String(), "name: All") {
		t.Errorf("the default profile should not be exported:\n%s", rec.Body.String())
	}
}

// TestConfigExportProfilesRoundTrip proves the exported YAML parses as a config
// file and re-seeds equivalent leaf and derived profiles into a fresh store.
func TestConfigExportProfilesRoundTrip(t *testing.T) {
	st, h := setup(t)
	if _, err := st.CreateProfile(t.Context(), "glm-only",
		store.KeyFilter{Mode: "include", Values: []string{"hyper"}},
		store.KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProfile(t.Context(), "combined",
		store.KeyFilter{}, store.KeyFilter{}, []string{"glm-only"}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/config", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	path := filepath.Join(t.TempDir(), "toll.yaml")
	if err := os.WriteFile(path, rec.Body.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOLL_CONFIG", path)
	t.Setenv("TOLL_UPSTREAM_URL", "")
	t.Setenv("TOLL_UPSTREAM_API_KEY", "")
	cfg, err := config.Load(t.Context(), config.Flags{})
	if err != nil {
		t.Fatalf("export did not parse as config: %v\n%s", err, rec.Body.String())
	}
	if len(cfg.Profiles) != 2 {
		t.Fatalf("parsed profiles = %+v", cfg.Profiles)
	}

	fresh, err := store.Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	seeds := make([]store.SeedProfile, 0, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		seeds = append(seeds, store.SeedProfile{
			Name:           p.Name,
			ProviderFilter: store.KeyFilter{Mode: p.ProviderFilter.Mode, Values: p.ProviderFilter.Values},
			ModelFilter:    store.KeyFilter{Mode: p.ModelFilter.Mode, Values: p.ModelFilter.Values},
			Parents:        p.Parents,
		})
	}
	if err := fresh.SeedProfiles(t.Context(), seeds); err != nil {
		t.Fatal(err)
	}
	got, err := fresh.ProfileByName(t.Context(), "glm-only")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderFilter.Mode != "include" || len(got.ProviderFilter.Values) != 1 ||
		got.ProviderFilter.Values[0] != "hyper" || got.ModelFilter.Mode != "none" {
		t.Errorf("round-tripped profile = %+v", got)
	}
	derived, err := fresh.ProfileByName(t.Context(), "combined")
	if err != nil {
		t.Fatal(err)
	}
	if len(derived.Parents) != 1 || derived.Parents[0] != "glm-only" {
		t.Errorf("round-tripped derived parents = %v", derived.Parents)
	}
}

// TestModelDisableEnable covers the disable/enable endpoints and that the
// disabled flag round-trips through the config export.
func TestModelDisableEnable(t *testing.T) {
	st, h := setup(t)

	upID, _ := st.UpsertUpstream(t.Context(), "hyper", "https://x/v1", "k", 300, 0)
	_, err := st.ReplaceModels(t.Context(), upID, []store.DiscoveredModel{{
		UpstreamModelID: "glm", GatewayID: "hyper/glm", DisplayName: "glm", Metadata: []byte(`{}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var modelID int64
	if err := st.DB().QueryRow(`SELECT id FROM models WHERE upstream_model_id = 'glm'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}

	list := func() (id int64, disabled bool) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/models", nil))
		var models []struct {
			ID       int64 `json:"id"`
			Disabled bool  `json:"disabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
			t.Fatal(err)
		}
		if len(models) != 1 {
			t.Fatalf("models = %d, want 1", len(models))
		}
		return models[0].ID, models[0].Disabled
	}
	post := func(suffix string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/models/"+strconv.FormatInt(modelID, 10)+suffix, nil))
		return rec.Code
	}

	if _, disabled := list(); disabled {
		t.Fatal("model should start enabled")
	}
	if code := post("/disable"); code != http.StatusNoContent {
		t.Fatalf("disable status = %d", code)
	}
	if _, disabled := list(); !disabled {
		t.Fatal("model should be disabled")
	}

	// Disabled models are still exported, flagged disabled.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/config", nil))
	var got struct {
		Upstreams []struct {
			Models []struct {
				Disabled bool `yaml:"disabled"`
			} `yaml:"models"`
		} `yaml:"upstreams"`
	}
	if err := yaml.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Upstreams) != 1 || len(got.Upstreams[0].Models) != 1 || !got.Upstreams[0].Models[0].Disabled {
		t.Fatalf("export did not flag disabled model: %s", rec.Body.String())
	}

	if code := post("/enable"); code != http.StatusNoContent {
		t.Fatalf("enable status = %d", code)
	}
	if _, disabled := list(); disabled {
		t.Fatal("model should be enabled again")
	}

	// Only disabled models carry the flag; enabled ones omit it.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/config", nil))
	if strings.Contains(rec.Body.String(), "disabled:") {
		t.Errorf("enabled model should omit disabled:\n%s", rec.Body.String())
	}

	// Unknown id → 404.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/models/9999/disable", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404", rec.Code)
	}
}

func TestProvidersCreateAndDelete(t *testing.T) {
	_, h := setup(t)

	// A fake upstream exposing GET /v1/models.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"glm","context_window":128000},{"id":"qwen"}]}`))
	}))
	defer up.Close()

	body := `{"name":"hyper","baseURL":"` + up.URL + `/v1","apiKey":"secret"}`
	req := httptest.NewRequest("POST", "/api/providers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ModelCount int    `json:"modelCount"`
		Warning    string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ModelCount != 2 || created.Warning != "" {
		t.Errorf("create response = %s", rec.Body.String())
	}

	// Models carry their upstream linkage and an id for deletion.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/models", nil))
	var models []struct {
		ID            int64  `json:"id"`
		Upstream      string `json:"upstream"`
		GatewayID     string `json:"gatewayId"`
		UpstreamModel string `json:"upstreamModelId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Upstream != "hyper" {
		t.Fatalf("unexpected models: %s", rec.Body.String())
	}

	// Delete one model.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/models/"+strconv.FormatInt(models[0].ID, 10), nil))
	if rec.Code != 204 {
		t.Fatalf("delete model status = %d: %s", rec.Code, rec.Body.String())
	}
	// Deleting again → 404.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/models/"+strconv.FormatInt(models[0].ID, 10), nil))
	if rec.Code != 404 {
		t.Fatalf("second delete status = %d, want 404", rec.Code)
	}

	// Delete the provider (and its remaining model via cascade).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/providers/hyper", nil))
	if rec.Code != 204 {
		t.Fatalf("delete provider status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/providers", nil))
	var providers []json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &providers)
	if len(providers) != 0 {
		t.Errorf("providers remain after delete: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/models", nil))
	var remaining []json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &remaining)
	if len(remaining) != 0 {
		t.Errorf("models remain after provider delete: %s", rec.Body.String())
	}
}

func TestProviderCreateValidation(t *testing.T) {
	_, h := setup(t)

	for name, body := range map[string]string{
		"missing fields": `{"name":"x"}`,
		"bad url":        `{"name":"x","baseURL":"ftp://x/v1","apiKey":"k"}`,
	} {
		req := httptest.NewRequest("POST", "/api/providers", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", name, rec.Code)
		}
	}
}

func TestModelsRefresh(t *testing.T) {
	st, h := setup(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-4.6","context_window":1000}]}`))
	}))
	defer upstream.Close()

	id, _ := st.UpsertUpstream(t.Context(), "zeph", upstream.URL+"/v1", "k", 300, 0)
	// A stale entry proves the refresh replaces the whole registry.
	if _, err := st.ReplaceModels(t.Context(), id, []store.DiscoveredModel{{
		UpstreamModelID: "stale", GatewayID: "stale", DisplayName: "stale", Metadata: []byte(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/models/refresh", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d: %s", rec.Code, rec.Body.String())
	}

	ms, err := st.ListModels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].GatewayID != "zeph/glm-4.6" || ms[0].UpstreamModelID != "glm-4.6" {
		t.Fatalf("refreshed models = %+v, want one zeph/glm-4.6", ms)
	}
}

func TestProviderDisableEnable(t *testing.T) {
	st, h := setup(t)
	if _, err := st.UpsertUpstream(t.Context(), "zeph", "https://z/v1", "k", 300, 0); err != nil {
		t.Fatal(err)
	}

	post := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		return rec.Code
	}

	if code := post("/api/providers/zeph/disable"); code != http.StatusNoContent {
		t.Fatalf("disable status = %d, want 204", code)
	}
	ups, _ := st.ListUpstreams(t.Context())
	if len(ups) != 1 || !ups[0].Disabled {
		t.Fatalf("provider not disabled: %+v", ups)
	}

	// The providers endpoint surfaces the flag for the UI.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/providers", nil))
	var list []struct {
		Name     string `json:"name"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Disabled {
		t.Errorf("providers response missing disabled: %s", rec.Body.String())
	}

	if code := post("/api/providers/zeph/enable"); code != http.StatusNoContent {
		t.Fatalf("enable status = %d, want 204", code)
	}
	ups, _ = st.ListUpstreams(t.Context())
	if len(ups) != 1 || ups[0].Disabled {
		t.Fatalf("provider not re-enabled: %+v", ups)
	}

	if code := post("/api/providers/missing/disable"); code != http.StatusNotFound {
		t.Fatalf("unknown provider status = %d, want 404", code)
	}
}

func TestSPAServesIndexAndFiles(t *testing.T) {
	_, h := setup(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Fatalf("index not served: %d %s", rec.Code, rec.Body.String())
	}

	// Client-side route falls back to index.html.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/keys", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Fatalf("route fallback failed: %d %s", rec.Code, rec.Body.String())
	}

	// Real assets are served from the embedded dist.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/"+assetName(t), nil))
	if rec.Code != 200 {
		t.Fatalf("asset not served: %d", rec.Code)
	}
}

// assetName returns a JS asset filename from the embedded dist manifest.
func assetName(t *testing.T) string {
	t.Helper()
	sub, err := fs.Sub(distFS, "web/dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			return e.Name()
		}
	}
	t.Fatal("no js assets in dist")
	return ""
}
