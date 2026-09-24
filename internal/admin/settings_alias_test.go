package admin

import (
	"net/http"
	"strconv"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

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
	st.EnsureConversation(t.Context(), "conv", keyID)
	tid, _ := st.CreateTranscript(t.Context(), "conv", "m", "m-upstream", `{"model":"m"}`)
	st.CompleteTranscript(t.Context(), tid, `{"ok":true}`, 200, store.UsageEvent{KeyID: keyID, UpstreamID: upID})
	if err := st.DeleteVirtualKey(t.Context(), "app"); err != nil {
		t.Fatal(err)
	}

	totalReqs := func(filter string) int64 {
		t.Helper()
		var v adminv1.GetUsageResponse
		rpcOK(t, h, "GetUsage", filter, &v)
		return v.GetTotals().GetRequests()
	}

	if n := totalReqs(`{}`); n != 1 {
		t.Errorf("no filter totals.requests = %d, want 1", n)
	}
	// include_deleted_keys replaces the old __deleted__ sentinel.
	if n := totalReqs(`{"filter": {"includeDeletedKeys": true}}`); n != 1 {
		t.Errorf("include_deleted_keys totals.requests = %d, want 1", n)
	}
	if n := totalReqs(`{"filter": {"keys": ["app"]}}`); n != 0 {
		t.Errorf("deleted key by name totals.requests = %d, want 0", n)
	}

	// Deleted-key request rows carry an empty key_name for the UI placeholder.
	var reqs adminv1.ListRequestsResponse
	rpcOK(t, h, "ListRequests", `{"filter": {"includeDeletedKeys": true}}`, &reqs)
	if len(reqs.GetRequests()) != 1 || reqs.GetRequests()[0].GetKeyName() != "" {
		t.Errorf("deleted-key request = %s", protojson.Format(&reqs))
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

	var got adminv1.RequestDetail
	rpcOK(t, h, "GetRequest", `{"id": `+strconv.FormatInt(id, 10)+`}`, &got)
	if got.GetContentStored() {
		t.Error("contentStored = true, want false when prompt storage is disabled")
	}
	if len(got.GetMessages()) != 0 {
		t.Errorf("messages = %d, want 0", len(got.GetMessages()))
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
