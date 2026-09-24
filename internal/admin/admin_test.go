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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
	"gopkg.in/yaml.v3"

	adminv1 "github.com/trfdeer/toll/gen/toll/admin/v1"
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

	var ups adminv1.ListProvidersResponse
	rpcOK(t, h, "ListProviders", `{}`, &ups)
	if ups.GetTotal() != 1 || len(ups.GetProviders()) != 1 ||
		ups.GetProviders()[0].GetName() != "hyper" ||
		ups.GetProviders()[0].GetBaseUrl() != "https://x/v1" ||
		ups.GetProviders()[0].GetModelCount() != 1 {
		t.Errorf("unexpected providers: %s", protojson.Format(&ups))
	}

	var models adminv1.ListModelsResponse
	rpcOK(t, h, "ListModels", `{}`, &models)
	if len(models.GetModels()) != 1 || models.GetModels()[0].GetGatewayId() != "hyper/glm" {
		t.Errorf("unexpected models: %s", protojson.Format(&models))
	}

	var usage adminv1.GetUsageResponse
	rpcOK(t, h, "GetUsage", `{}`, &usage)
	if usage.GetTotals().GetRequests() != 0 {
		t.Errorf("unexpected usage: %s", protojson.Format(&usage))
	}

	// Requests list (empty).
	var reqs adminv1.ListRequestsResponse
	rpcOK(t, h, "ListRequests", `{}`, &reqs)
	if len(reqs.GetRequests()) != 0 {
		t.Errorf("unexpected requests: %s", protojson.Format(&reqs))
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

	// Requests narrowed by key: the completed one carries its duration.
	var reqs adminv1.ListRequestsResponse
	rpcOK(t, h, "ListRequests", `{"filter": {"keys": ["app"]}}`, &reqs)
	if len(reqs.GetRequests()) != 1 || reqs.GetTotal() != 1 {
		t.Errorf("key=app requests = %s", protojson.Format(&reqs))
	}
	if len(reqs.GetRequests()) == 1 && reqs.GetRequests()[0].GetDuration() == nil {
		t.Error("completed request is missing duration")
	}
	rpcOK(t, h, "ListRequests", `{"filter": {"keys": ["nope"]}}`, &reqs)
	if len(reqs.GetRequests()) != 0 {
		t.Errorf("key=nope requests = %s", protojson.Format(&reqs))
	}

	// A lower bound in the future excludes everything (closed interval).
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rpcOK(t, h, "ListRequests", `{"filter": {"from": "`+future+`"}}`, &reqs)
	if len(reqs.GetRequests()) != 0 {
		t.Errorf("future from requests = %s", protojson.Format(&reqs))
	}
	var usage adminv1.GetUsageResponse
	rpcOK(t, h, "GetUsage", `{"filter": {"from": "`+future+`"}}`, &usage)
	if usage.GetTotals().GetRequests() != 0 {
		t.Errorf("future from usage totals = %s", protojson.Format(&usage))
	}

	// Malformed parameters fail loudly.
	rpcFail(t, h, "ListRequests", `{"params": {"limit": 1001}}`, http.StatusBadRequest, "invalid_argument")
}

func TestRequestDetailBuildsConversation(t *testing.T) {
	st, h := setup(t)

	keyID, _ := st.CreateVirtualKey(t.Context(), "app", "hash", 1)
	st.EnsureConversation(t.Context(), "conv", keyID)
	reqBody := `{"model":"m","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`
	id, _ := st.CreateTranscript(t.Context(), "conv", "m", "m-upstream", reqBody)
	respBody := `{"choices":[{"message":{"role":"assistant","content":"hello!"}}]}`
	st.CompleteTranscript(t.Context(), id, respBody, 200, store.UsageEvent{KeyID: keyID, UpstreamID: 1})

	var got adminv1.RequestDetail
	rpcOK(t, h, "GetRequest", `{"id": `+strconv.FormatInt(id, 10)+`}`, &got)
	if len(got.GetMessages()) != 3 {
		t.Fatalf("messages = %d, want 3: %s", len(got.GetMessages()), protojson.Format(&got))
	}
	want := []struct{ role, content string }{
		{"system", "be nice"},
		{"user", "hi"},
		{"assistant", "hello!"},
	}
	for i, w := range want {
		if got.GetMessages()[i].GetRole() != w.role || got.GetMessages()[i].GetContent() != w.content {
			t.Errorf("message %d = %s, want %s/%q", i, protojson.Format(got.GetMessages()[i]), w.role, w.content)
		}
	}
	if got.GetDuration() == nil {
		t.Error("duration is nil for a completed request")
	}

	// Unknown id → not_found.
	rpcFail(t, h, "GetRequest", `{"id": 9999}`, http.StatusNotFound, "not_found")
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

	body := getConfigYAML(t, h)

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
	if err := yaml.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("export is not valid YAML: %v\n%s", err, body)
	}
	if len(got.Upstreams) != 1 {
		t.Fatalf("upstreams = %d, want 1\n%s", len(got.Upstreams), body)
	}
	u := got.Upstreams[0]
	if u.Name != "hyper" || u.URL != "https://hyper.charm.land/v1" || u.APIKeyEnv != "HYPER_API_KEY" {
		t.Errorf("unexpected upstream: %+v", u)
	}
	if strings.Contains(body, "secret") {
		t.Errorf("export leaked the api key: %s", body)
	}
	if len(u.Models) != 2 {
		t.Fatalf("models = %d, want 2\n%s", len(u.Models), body)
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
		t.Fatalf("profiles = %d, want 1\n%s", len(got.Profiles), body)
	}
	p := got.Profiles[0]
	if p.Name != "glm-only" ||
		p.ProviderFilter.Mode != "include" || len(p.ProviderFilter.Values) != 1 || p.ProviderFilter.Values[0] != "hyper" ||
		p.ModelFilter.Mode != "exclude" || len(p.ModelFilter.Values) != 1 || p.ModelFilter.Values[0] != "hyper/hidden" {
		t.Errorf("unexpected exported profile: %+v", p)
	}
	if strings.Contains(body, "name: All") {
		t.Errorf("the default profile should not be exported:\n%s", body)
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

	body := getConfigYAML(t, h)
	path := filepath.Join(t.TempDir(), "toll.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOLL_CONFIG", path)
	t.Setenv("TOLL_UPSTREAM_URL", "")
	t.Setenv("TOLL_UPSTREAM_API_KEY", "")
	cfg, err := config.Load(t.Context(), config.Flags{})
	if err != nil {
		t.Fatalf("export did not parse as config: %v\n%s", err, body)
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
		var modelsBody adminv1.ListModelsResponse
		rpcOK(t, h, "ListModels", `{}`, &modelsBody)
		if len(modelsBody.GetModels()) != 1 {
			t.Fatalf("models = %d, want 1", len(modelsBody.GetModels()))
		}
		m := modelsBody.GetModels()[0]
		return m.GetId(), m.GetDisabled()
	}
	setDisabled := func(id int64, disabled bool) {
		t.Helper()
		body := `{"id":` + strconv.FormatInt(id, 10) + `,"disabled":` + strconv.FormatBool(disabled) + `}`
		var m adminv1.Model
		rpcOK(t, h, "UpdateModel", body, &m)
	}

	if _, disabled := list(); disabled {
		t.Fatal("model should start enabled")
	}
	setDisabled(modelID, true)
	if _, disabled := list(); !disabled {
		t.Fatal("model should be disabled")
	}

	// Disabled models are still exported, flagged disabled.
	body := getConfigYAML(t, h)
	var got struct {
		Upstreams []struct {
			Models []struct {
				Disabled bool `yaml:"disabled"`
			} `yaml:"models"`
		} `yaml:"upstreams"`
	}
	if err := yaml.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Upstreams) != 1 || len(got.Upstreams[0].Models) != 1 || !got.Upstreams[0].Models[0].Disabled {
		t.Fatalf("export did not flag disabled model: %s", body)
	}

	setDisabled(modelID, false)
	if _, disabled := list(); disabled {
		t.Fatal("model should be enabled again")
	}

	// Only disabled models carry the flag; enabled ones omit it.
	body = getConfigYAML(t, h)
	if strings.Contains(body, "disabled:") {
		t.Errorf("enabled model should omit disabled:\n%s", body)
	}

	// Unknown id → not_found.
	rpcFail(t, h, "UpdateModel", `{"id": 9999, "disabled": true}`, http.StatusNotFound, "not_found")
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
	var created adminv1.CreateProviderResponse
	rpcOK(t, h, "CreateProvider", body, &created)
	if created.GetProvider().GetModelCount() != 2 || created.GetWarning() != "" ||
		!created.GetProvider().GetReachable() || created.GetProvider().GetLastSyncedAt() == nil {
		t.Errorf("create response = %s", protojson.Format(&created))
	}

	// Models carry their upstream linkage and an id for deletion.
	var modelsBody adminv1.ListModelsResponse
	rpcOK(t, h, "ListModels", `{}`, &modelsBody)
	models := modelsBody.GetModels()
	if len(models) != 2 || models[0].GetUpstream() != "hyper" {
		t.Fatalf("unexpected models: %s", protojson.Format(&modelsBody))
	}

	// Delete one model.
	delID := strconv.FormatInt(models[0].GetId(), 10)
	rpcOK(t, h, "DeleteModel", `{"id":`+delID+`}`, new(emptypb.Empty))
	// Deleting again → not_found.
	rpcFail(t, h, "DeleteModel", `{"id":`+delID+`}`, http.StatusNotFound, "not_found")

	// Delete the provider (and its remaining model via cascade).
	rpcOK(t, h, "DeleteProvider", `{"name":"hyper"}`, new(emptypb.Empty))
	var providersBody adminv1.ListProvidersResponse
	rpcOK(t, h, "ListProviders", `{}`, &providersBody)
	if len(providersBody.GetProviders()) != 0 {
		t.Errorf("providers remain after delete: %s", protojson.Format(&providersBody))
	}
	var remainingBody adminv1.ListModelsResponse
	rpcOK(t, h, "ListModels", `{}`, &remainingBody)
	if len(remainingBody.GetModels()) != 0 {
		t.Errorf("models remain after provider delete: %s", protojson.Format(&remainingBody))
	}
}

func TestProviderCreateValidation(t *testing.T) {
	_, h := setup(t)

	for name, body := range map[string]string{
		"missing fields": `{"name":"x"}`,
		"bad url":        `{"name":"x","baseURL":"ftp://x/v1","apiKey":"k"}`,
	} {
		rec := rpc(t, h, "CreateProvider", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
		wantConnectCode(t, rec, "invalid_argument")
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

	var res adminv1.RefreshModelsResponse
	rpcOK(t, h, "RefreshModels", `{}`, &res)
	if res.GetProviders() != 1 || res.GetModels() != 1 || len(res.GetWarnings()) != 0 {
		t.Fatalf("refresh response = %s", protojson.Format(&res))
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

	setDisabled := func(name string, disabled bool) {
		t.Helper()
		body := `{"name":"` + name + `","disabled":` + strconv.FormatBool(disabled) + `}`
		var p adminv1.Provider
		rpcOK(t, h, "UpdateProvider", body, &p)
	}

	setDisabled("zeph", true)
	ups, _ := st.ListUpstreams(t.Context())
	if len(ups) != 1 || !ups[0].Disabled {
		t.Fatalf("provider not disabled: %+v", ups)
	}

	// The providers endpoint surfaces the flag for the UI.
	var listBody adminv1.ListProvidersResponse
	rpcOK(t, h, "ListProviders", `{}`, &listBody)
	list := listBody.GetProviders()
	if len(list) != 1 || !list[0].GetDisabled() {
		t.Errorf("providers response missing disabled: %s", protojson.Format(&listBody))
	}

	setDisabled("zeph", false)
	ups, _ = st.ListUpstreams(t.Context())
	if len(ups) != 1 || ups[0].Disabled {
		t.Fatalf("provider not re-enabled: %+v", ups)
	}

	rpcFail(t, h, "UpdateProvider", `{"name":"missing","disabled":true}`, http.StatusNotFound, "not_found")
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
