package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	adminv1 "github.com/trfdeer/toll/gen/toll/admin/v1"
	"github.com/trfdeer/toll/internal/store"
)

func TestSettingsRoundTrip(t *testing.T) {
	_, h := setup(t)

	get := func() *adminv1.Settings {
		t.Helper()
		var v adminv1.Settings
		rpcOK(t, h, "GetSettings", `{}`, &v)
		return &v
	}

	if !get().GetStorePrompts() {
		t.Error("default storePrompts = false, want true")
	}

	// An absent field is left unchanged; false persists.
	var v adminv1.Settings
	rpcOK(t, h, "UpdateSettings", `{}`, &v)
	if !v.GetStorePrompts() {
		t.Error("absent update changed storePrompts")
	}
	rpcOK(t, h, "UpdateSettings", `{"storePrompts":false}`, &v)
	if v.GetStorePrompts() {
		t.Error("storePrompts still true after update false")
	}
	if get().GetStorePrompts() {
		t.Error("storePrompts still true after update false")
	}
	rpcOK(t, h, "UpdateSettings", `{"storePrompts":true}`, &v)
	if !get().GetStorePrompts() {
		t.Error("storePrompts still false after update true")
	}
}

func TestDeletedKeysUsageEndpoint(t *testing.T) {
	st, h := setup(t)

	upID, _ := st.UpsertUpstream(t.Context(), "hyper", "https://x/v1", "k", 300, 0)
	keyID, _ := st.CreateVirtualKey(t.Context(), "app", "hash", 1)
	st.RecordUsage(t.Context(), store.UsageEvent{
		KeyID: keyID, UpstreamID: upID, GatewayModel: "m", UpstreamModel: "m",
	})
	if err := st.DeleteVirtualKey(t.Context(), "app"); err != nil {
		t.Fatal(err)
	}

	total := func(query string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/usage%s = %d: %s", query, rec.Code, rec.Body.String())
		}
		var v struct {
			TotalReqs int `json:"totalReqs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v.TotalReqs
	}

	if n := total(""); n != 1 {
		t.Errorf("no filter totalReqs = %d, want 1", n)
	}
	if n := total("?key=" + deletedKeysSentinel); n != 1 {
		t.Errorf("deleted sentinel totalReqs = %d, want 1", n)
	}
	if n := total("?key=app"); n != 0 {
		t.Errorf("deleted key by name totalReqs = %d, want 0", n)
	}
}

func TestRequestDetailReportsMissingContent(t *testing.T) {
	st, h := setup(t)

	if err := st.SetSetting(t.Context(), store.SettingStorePrompts, "false"); err != nil {
		t.Fatal(err)
	}
	keyID, _ := st.CreateVirtualKey(t.Context(), "app", "hash", 1)
	st.EnsureConversation(t.Context(), "conv", keyID)
	id, _ := st.CreateTranscript(t.Context(), "conv", "m", "m-up", `{"messages":[]}`)
	st.CompleteTranscript(t.Context(), id, `{}`, 200, store.UsageEvent{KeyID: keyID, UpstreamID: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests/"+strconv.FormatInt(id, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got requestDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ContentStored {
		t.Error("contentStored = true, want false when prompt storage is disabled")
	}
	if len(got.Messages) != 0 {
		t.Errorf("messages = %d, want 0", len(got.Messages))
	}
}

func TestModelAliasEndpoint(t *testing.T) {
	st, h := setup(t)

	upID, _ := st.UpsertUpstream(t.Context(), "hyper", "https://x/v1", "k", 300, 0)
	st.ReplaceModels(t.Context(), upID, []store.DiscoveredModel{
		{UpstreamModelID: "glm", GatewayID: "hyper/glm", DisplayName: "GLM", Metadata: []byte(`{}`)},
		{UpstreamModelID: "other", GatewayID: "hyper/other", DisplayName: "Other", Metadata: []byte(`{}`)},
	})

	list := func() map[string]*adminv1.Model {
		t.Helper()
		var body adminv1.ListModelsResponse
		rpcOK(t, h, "ListModels", `{}`, &body)
		out := make(map[string]*adminv1.Model, len(body.GetModels()))
		for _, m := range body.GetModels() {
			out[m.GetUpstreamModelId()] = m
		}
		return out
	}
	setAlias := func(id int64, alias string) {
		t.Helper()
		var m adminv1.Model
		rpcOK(t, h, "UpdateModel",
			`{"id":`+strconv.FormatInt(id, 10)+`,"alias":`+strconv.Quote(alias)+`}`, &m)
	}

	glm := list()["glm"]
	setAlias(glm.GetId(), "gpt-4o")
	if got := list()["glm"]; got.GetGatewayId() != "gpt-4o" || got.GetAlias() != "gpt-4o" {
		t.Errorf("after set: gatewayId=%q alias=%q", got.GetGatewayId(), got.GetAlias())
	}

	other := list()["other"]
	rpcFail(t, h, "UpdateModel",
		`{"id":`+strconv.FormatInt(other.GetId(), 10)+`,"alias":"gpt-4o"}`,
		http.StatusConflict, "already_exists")

	setAlias(glm.GetId(), "")
	if got := list()["glm"]; got.GetGatewayId() != "hyper/glm" || got.GetAlias() != "" {
		t.Errorf("after clear: gatewayId=%q alias=%q", got.GetGatewayId(), got.GetAlias())
	}
}
