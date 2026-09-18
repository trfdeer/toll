package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/trfdeer/toll/internal/store"
)

func TestSettingsRoundTrip(t *testing.T) {
	_, h := setup(t)

	get := func() settingsView {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/settings", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/settings = %d: %s", rec.Code, rec.Body.String())
		}
		var v settingsView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	if !get().StorePrompts {
		t.Error("default storePrompts = false, want true")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"storePrompts":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings = %d: %s", rec.Code, rec.Body.String())
	}
	if get().StorePrompts {
		t.Error("storePrompts still true after PUT false")
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

	type model struct {
		ID              int64  `json:"id"`
		UpstreamModelID string `json:"upstreamModelId"`
		GatewayID       string `json:"gatewayId"`
		Alias           string `json:"alias"`
	}
	list := func() map[string]model {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/models = %d: %s", rec.Code, rec.Body.String())
		}
		var ms []model
		if err := json.Unmarshal(rec.Body.Bytes(), &ms); err != nil {
			t.Fatal(err)
		}
		out := make(map[string]model, len(ms))
		for _, m := range ms {
			out[m.UpstreamModelID] = m
		}
		return out
	}
	put := func(id int64, alias string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/api/models/"+strconv.FormatInt(id, 10)+"/alias",
			strings.NewReader(`{"alias":`+strconv.Quote(alias)+`}`))
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	glm := list()["glm"]
	if code := put(glm.ID, "gpt-4o"); code != http.StatusNoContent {
		t.Fatalf("set alias status = %d, want 204", code)
	}
	if got := list()["glm"]; got.GatewayID != "gpt-4o" || got.Alias != "gpt-4o" {
		t.Errorf("after set: gatewayId=%q alias=%q", got.GatewayID, got.Alias)
	}

	other := list()["other"]
	if code := put(other.ID, "gpt-4o"); code != http.StatusUnprocessableEntity {
		t.Errorf("conflicting alias status = %d, want 422", code)
	}

	if code := put(glm.ID, ""); code != http.StatusNoContent {
		t.Fatalf("clear alias status = %d, want 204", code)
	}
	if got := list()["glm"]; got.GatewayID != "hyper/glm" || got.Alias != "" {
		t.Errorf("after clear: gatewayId=%q alias=%q", got.GatewayID, got.Alias)
	}
}
