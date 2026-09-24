// Tests for the ConnectRPC admin surface (MIGRATION.md phase 1): the virtual
// keys pilot, exercised over Connect-protocol JSON against the mounted
// handler, alongside the still-serving REST routes.
package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	adminv1 "github.com/trfdeer/toll/gen/toll/admin/v1"
	"github.com/trfdeer/toll/internal/keys"
	"github.com/trfdeer/toll/internal/store"
)

// rpc calls one ConnectRPC with a proto3 JSON body and returns the response
// recorder. Errors come back as Connect's {"code": ..., "message": ...} JSON
// with the mapped HTTP status.
func rpc(t *testing.T, h http.Handler, procedure, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/toll.admin.v1.AdminService/"+procedure, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

// rpcOK asserts a 200 and unmarshals the response into out.
func rpcOK(t *testing.T, h http.Handler, procedure, body string, out proto.Message) {
	t.Helper()
	rec := rpc(t, h, procedure, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d: %s", procedure, rec.Code, rec.Body.String())
	}
	if err := protojson.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("%s: decode response: %v", procedure, err)
	}
}

// wantConnectCode asserts the response carries the given Connect error code.
func wantConnectCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
	}
	if body.Code != want {
		t.Fatalf("error code = %q (status %d), want %q: %s", body.Code, rec.Code, want, rec.Body.String())
	}
}

// rpcFail asserts the RPC answers the given HTTP status and Connect code.
func rpcFail(t *testing.T, h http.Handler, procedure, body string, wantStatus int, wantCode string) {
	t.Helper()
	rec := rpc(t, h, procedure, body)
	if rec.Code != wantStatus {
		t.Fatalf("%s = %d, want %d: %s", procedure, rec.Code, wantStatus, rec.Body.String())
	}
	wantConnectCode(t, rec, wantCode)
}

func TestConnectKeysLifecycle(t *testing.T) {
	st, h := setup(t)
	ctx := t.Context()

	if _, err := st.CreateProfile(ctx, "restricted", store.KeyFilter{}, store.KeyFilter{}, nil); err != nil {
		t.Fatal(err)
	}

	// Create returns the resource next to the one-time plaintext.
	var created adminv1.CreateKeyResponse
	rpcOK(t, h, "CreateKey", `{"name": "app", "profile": "restricted"}`, &created)
	if created.GetKey().GetName() != "app" || created.GetKey().GetProfile() != "restricted" ||
		created.GetKey().GetRevoked() || created.GetKey().GetPaused() {
		t.Errorf("created key = %s", protojson.Format(created.GetKey()))
	}
	if created.GetPlaintext() == "" {
		t.Error("CreateKey returned no plaintext")
	}

	// The plaintext authenticates right away.
	if _, err := st.KeyByHash(ctx, keys.Hash(created.GetPlaintext())); err != nil {
		t.Errorf("new key does not resolve: %v", err)
	}

	// Duplicate name.
	rec := rpc(t, h, "CreateKey", `{"name": "app"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	wantConnectCode(t, rec, "already_exists")

	// Unknown profile.
	rec = rpc(t, h, "CreateKey", `{"name": "bad", "profile": "nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown profile = %d, want 404", rec.Code)
	}
	wantConnectCode(t, rec, "not_found")

	// A second key to test rename conflicts against.
	rpcOK(t, h, "CreateKey", `{"name": "other"}`, new(adminv1.CreateKeyResponse))

	// PATCH semantics: paused only; name and profile are left unchanged.
	var updated adminv1.VirtualKey
	rpcOK(t, h, "UpdateKey", `{"name": "app", "paused": true}`, &updated)
	if updated.GetName() != "app" || updated.GetProfile() != "restricted" || !updated.GetPaused() {
		t.Errorf("paused-only update = %s", protojson.Format(&updated))
	}

	// The empty profile string resets the key to the All profile.
	rpcOK(t, h, "UpdateKey", `{"name": "app", "profile": ""}`, &updated)
	if updated.GetProfile() != store.DefaultProfileName || !updated.GetPaused() {
		t.Errorf("profile reset = %s", protojson.Format(&updated))
	}

	// Rename.
	rpcOK(t, h, "UpdateKey", `{"name": "app", "newName": "app2"}`, &updated)
	if updated.GetName() != "app2" || updated.GetProfile() != store.DefaultProfileName {
		t.Errorf("rename = %s", protojson.Format(&updated))
	}
	rec = rpc(t, h, "UpdateKey", `{"name": "app2", "newName": "other"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("rename onto taken name = %d, want 409", rec.Code)
	}
	wantConnectCode(t, rec, "already_exists")

	// Resume, so the rotation checks below can authenticate the key.
	rpcOK(t, h, "UpdateKey", `{"name": "app2", "paused": false}`, &updated)

	// Rotate: fresh secret under the same name and profile; the old material
	// dies immediately.
	var rotated adminv1.RotateKeyResponse
	rpcOK(t, h, "RotateKey", `{"name": "app2"}`, &rotated)
	if rotated.GetKey().GetName() != "app2" || rotated.GetPlaintext() == "" {
		t.Errorf("rotate = %s", protojson.Format(&rotated))
	}
	if _, err := st.KeyByHash(ctx, keys.Hash(created.GetPlaintext())); !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("old secret still resolves: %v", err)
	}
	vk, err := st.KeyByHash(ctx, keys.Hash(rotated.GetPlaintext()))
	if err != nil || vk.Name != "app2" {
		t.Errorf("new secret = (%+v, %v)", vk, err)
	}

	// Revoke; revoking again is a no-op, revoking an unknown key is not_found.
	var empty emptypb.Empty
	rpcOK(t, h, "RevokeKey", `{"name": "app2"}`, &empty)
	rpcOK(t, h, "RevokeKey", `{"name": "app2"}`, &empty)
	rec = rpc(t, h, "RevokeKey", `{"name": "missing"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("revoke unknown = %d, want 404", rec.Code)
	}
	wantConnectCode(t, rec, "not_found")

	// Pausing a revoked key stays a silent no-op.
	rpcOK(t, h, "RevokeKey", `{"name": "other"}`, &empty)
	rpcOK(t, h, "UpdateKey", `{"name": "other", "paused": true}`, &updated)
	if !updated.GetRevoked() || updated.GetPaused() {
		t.Errorf("pause revoked key = %s", protojson.Format(&updated))
	}

	// Delete; the second delete is not_found.
	rpcOK(t, h, "DeleteKey", `{"name": "app2"}`, &empty)
	rec = rpc(t, h, "DeleteKey", `{"name": "app2"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete deleted = %d, want 404", rec.Code)
	}
	wantConnectCode(t, rec, "not_found")
}

func TestConnectListKeysParams(t *testing.T) {
	st, h := setup(t)
	ctx := t.Context()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := st.CreateVirtualKey(ctx, name, "hash-"+name, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PauseVirtualKey(ctx, "beta"); err != nil {
		t.Fatal(err)
	}

	var list adminv1.ListKeysResponse
	rpcOK(t, h, "ListKeys", `{}`, &list)
	if list.GetTotal() != 3 || len(list.GetKeys()) != 3 {
		t.Fatalf("list = %d rows, total %d", len(list.GetKeys()), list.GetTotal())
	}

	// Paging.
	rpcOK(t, h, "ListKeys", `{"params": {"limit": 2, "offset": 1}}`, &list)
	if len(list.GetKeys()) != 2 || list.GetKeys()[0].GetName() != "beta" || list.GetTotal() != 3 {
		t.Errorf("page = %s", protojson.Format(&list))
	}

	// Sorting.
	rpcOK(t, h, "ListKeys", `{"params": {"sort": "name", "dir": "SORT_DIRECTION_DESC"}}`, &list)
	if list.GetKeys()[0].GetName() != "gamma" {
		t.Errorf("desc sort first = %s", list.GetKeys()[0].GetName())
	}

	// Filtering: text op and boolean columns.
	rpcOK(t, h, "ListKeys", `{"params": {"filter": {"name": {"conditions": [{"op": "FILTER_OP_STARTS_WITH", "values": ["al"]}]}}}}`, &list)
	if len(list.GetKeys()) != 1 || list.GetKeys()[0].GetName() != "alpha" {
		t.Errorf("name filter = %s", protojson.Format(&list))
	}
	rpcOK(t, h, "ListKeys", `{"params": {"filter": {"revoked": {"conditions": [{"values": ["true"]}]}}}}`, &list)
	if len(list.GetKeys()) != 0 {
		t.Errorf("revoked filter matched revoked keys: %s", protojson.Format(&list))
	}
	rpcOK(t, h, "ListKeys", `{"params": {"filter": {"paused": {"conditions": [{"values": ["true"]}]}}}}`, &list)
	if len(list.GetKeys()) != 1 || list.GetKeys()[0].GetName() != "beta" {
		t.Errorf("paused filter = %s", protojson.Format(&list))
	}

	// Multi-condition filters: the UI's condition1 AND/OR condition2 shape
	// survives the trip (the dropped-condition2 bug is fixed).
	rpcOK(t, h, "ListKeys", `{"params": {"filter": {"name": {"conditions": [{"op": "FILTER_OP_STARTS_WITH", "values": ["a"]}, {"op": "FILTER_OP_STARTS_WITH", "values": ["g"]}], "join": "FILTER_JOIN_AND"}}}}`, &list)
	if len(list.GetKeys()) != 0 {
		t.Errorf("AND conditions matched: %s", protojson.Format(&list))
	}
	rpcOK(t, h, "ListKeys", `{"params": {"filter": {"name": {"conditions": [{"op": "FILTER_OP_STARTS_WITH", "values": ["a"]}, {"op": "FILTER_OP_STARTS_WITH", "values": ["g"]}], "join": "FILTER_JOIN_OR"}}}}`, &list)
	if len(list.GetKeys()) != 2 {
		t.Errorf("OR conditions = %s", protojson.Format(&list))
	}

	// Numeric ops compare as REAL: beta has 0 requests… (keys carry no
	// numeric columns; the requests tests cover GT/BETWEEN.)

	// Numeric and multi-condition ops on the requests surface are covered by
	// TestConnectRequestsFilters; the keys table has no numeric columns.

	// Malformed parameters fail loudly (proto/README.md item 8).
	for _, tc := range []struct {
		name   string
		params string
	}{
		{"limit above max", `{"params": {"limit": 1001}}`},
		{"negative limit", `{"params": {"limit": -1}}`},
		{"negative offset", `{"params": {"offset": -1}}`},
		{"zero conditions", `{"params": {"filter": {"name": {"conditions": []}}}}`},
		{"unknown column", `{"params": {"filter": {"nope": {"conditions": [{"values": ["x"]}]}}}}`},
		{"unknown sort column", `{"params": {"sort": "nope"}}`},
		{"blank text-op value", `{"params": {"filter": {"name": {"conditions": [{"op": "FILTER_OP_CONTAINS", "values": ["  "]}]}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rpc(t, h, "ListKeys", tc.params)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			wantConnectCode(t, rec, "invalid_argument")
		})
	}
}

// TestConnectUnmigratedRPCs was retired when the last planned RPCs (the
// phase-5 stats surface) cut over; the connect surface now covers the whole
// schema. New RPCs should pin their strangle window here when added.

// TestConnectRequestsFilters covers the numeric filter ops (GT/BETWEEN),
// multi-condition joins over requests columns and the int64 token sums.
func TestConnectRequestsFilters(t *testing.T) {
	st, h := setup(t)

	upID, _ := st.UpsertUpstream(t.Context(), "hyper", "https://x/v1", "k", 300, 0)
	keyID, _ := st.CreateVirtualKey(t.Context(), "app", "hash", 1)
	record := func(model string, prompt, cached, completion int64) {
		t.Helper()
		st.EnsureConversation(t.Context(), "conv-"+model, keyID)
		id, err := st.CreateTranscript(t.Context(), "conv-"+model, model, model, `{"model":"`+model+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		st.CompleteTranscript(t.Context(), id, `{"ok":true}`, 200, store.UsageEvent{
			KeyID: keyID, UpstreamID: upID, GatewayModel: model, UpstreamModel: model,
			PromptTokens: int(prompt), CachedTokens: int(cached), CompletionToken: int(completion),
		})
		if err := st.RecordUsage(t.Context(), store.UsageEvent{
			KeyID: keyID, UpstreamID: upID, GatewayModel: model, UpstreamModel: model,
			PromptTokens: int(prompt), CachedTokens: int(cached), CompletionToken: int(completion),
		}); err != nil {
			t.Fatal(err)
		}
	}
	record("small", 10, 0, 5)
	record("large", 5000, 100, 900)

	total := func(filter string) int {
		t.Helper()
		var res adminv1.ListRequestsResponse
		rpcOK(t, h, "ListRequests", filter, &res)
		return len(res.GetRequests())
	}

	// Numeric ops compare as REAL.
	if n := total(`{"params": {"filter": {"prompt": {"conditions": [{"op": "FILTER_OP_GT", "values": ["999"]}]}}}}`); n != 1 {
		t.Errorf("prompt GT 999 rows = %d, want 1", n)
	}
	if n := total(`{"params": {"filter": {"prompt": {"conditions": [{"op": "FILTER_OP_BETWEEN", "values": ["10", "100"]}]}}}}`); n != 1 {
		t.Errorf("prompt BETWEEN 10..100 rows = %d, want 1", n)
	}
	if n := total(`{"params": {"filter": {"prompt": {"conditions": [{"op": "FILTER_OP_BETWEEN", "values": ["", "20"]}]}}}}`); n != 1 {
		t.Errorf("prompt BETWEEN unbounded..20 rows = %d, want 1", n)
	}
	rpcFail(t, h, "ListRequests", `{"params": {"filter": {"prompt": {"conditions": [{"op": "FILTER_OP_GT", "values": ["abc"]}]}}}}`, http.StatusBadRequest, "invalid_argument")
	// A numeric op never matches a blank cell.
	if n := total(`{"params": {"filter": {"cost": {"conditions": [{"op": "FILTER_OP_GT", "values": ["-1"]}]}}}}`); n != 0 {
		t.Errorf("cost GT -1 rows = %d, want 0", n)
	}

	// Two conditions on one column, ANDed.
	if n := total(`{"params": {"filter": {"prompt": {"conditions": [{"op": "FILTER_OP_GTE", "values": ["1"]}, {"op": "FILTER_OP_LTE", "values": ["20"]}], "join": "FILTER_JOIN_AND"}}}}`); n != 1 {
		t.Errorf("prompt AND rows = %d, want 1", n)
	}
	// Two columns, ANDed across the filter map.
	if n := total(`{"params": {"filter": {"model": {"conditions": [{"op": "FILTER_OP_STARTS_WITH", "values": ["large"]}]}, "status": {"conditions": [{"op": "FILTER_OP_EQUALS", "values": ["200"]}]}}}}`); n != 1 {
		t.Errorf("model AND status rows = %d, want 1", n)
	}

	// Token sums are int64.
	var usage adminv1.GetUsageResponse
	rpcOK(t, h, "GetUsage", `{}`, &usage)
	if got, want := usage.GetTotals().GetPromptTokens(), int64(5010); got != want {
		t.Errorf("totals.promptTokens = %d, want %d", got, want)
	}
	if usage.GetTotal() != 2 {
		t.Errorf("usage total = %d, want 2", usage.GetTotal())
	}
}

// TestConnectUsageSeriesAndFilterValues covers the stats surface end to end:
// bucketed series with grouping, the filter-value dropdown queries and the
// client-error paths.
func TestConnectUsageSeriesAndFilterValues(t *testing.T) {
	st, h := setup(t)

	upID, _ := st.UpsertUpstream(t.Context(), "hyper", "https://x/v1", "k", 300, 0)
	keyID, _ := st.CreateVirtualKey(t.Context(), "app", "hash", 1)
	for _, tc := range []struct {
		model string
		cost  float64
	}{
		{"m1", 0.5},
		{"m2", 2.0},
	} {
		if err := st.RecordUsage(t.Context(), store.UsageEvent{
			KeyID: keyID, UpstreamID: upID, GatewayModel: tc.model, UpstreamModel: tc.model,
			CostUSD: &tc.cost,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A one-hour window around now, 30-minute buckets, grouped by model and
	// trimmed to the costliest group.
	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	var res adminv1.GetUsageSeriesResponse
	rpcOK(t, h, "GetUsageSeries", `{
		"filter": {"from": "`+from+`", "to": "`+to+`"},
		"bucketSize": "1800s",
		"groupBy": "SERIES_GROUPING_MODEL",
		"topGroups": 1
	}`, &res)
	// m2 (cost 2.0) keeps its line; m1 rolls into "Other".
	if len(res.GetSeries()) != 2 || res.GetSeries()[0].GetLabel() != "m2" ||
		res.GetSeries()[1].GetLabel() != "Other" {
		t.Fatalf("series = %s", protojson.Format(&res))
	}
	var m2Reqs int64
	for _, p := range res.GetSeries()[0].GetPoints() {
		m2Reqs += p.GetRequests()
	}
	if m2Reqs != 1 {
		t.Errorf("m2 series requests = %d, want 1", m2Reqs)
	}
	if res.GetTotals().GetRequests() != 2 {
		t.Errorf("totals = %s", protojson.Format(res.GetTotals()))
	}

	// An unknown filter column is a client error. (An unknown groupBy enum
	// name can't be pinned here: connect's protojson decoder discards
	// unknown enum names, so it degrades to the ungrouped series.)
	rpcFail(t, h, "ListFilterValues", `{"column": "bogus"}`,
		http.StatusBadRequest, "invalid_argument")

	var fv adminv1.ListFilterValuesResponse
	rpcOK(t, h, "ListFilterValues", `{"column": "model", "query": "2"}`, &fv)
	if len(fv.GetValues()) != 1 || fv.GetValues()[0] != "m2" {
		t.Errorf("model type-ahead = %s", protojson.Format(&fv))
	}
	rpcOK(t, h, "ListFilterValues", `{"column": "key"}`, &fv)
	if len(fv.GetValues()) != 1 || fv.GetValues()[0] != "app" {
		t.Errorf("key values = %s", protojson.Format(&fv))
	}
}

// getConfigYAML calls the ExportConfig RPC and returns the YAML document.
func getConfigYAML(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := rpc(t, h, "ExportConfig", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("ExportConfig = %d: %s", rec.Code, rec.Body.String())
	}
	var res adminv1.ExportConfigResponse
	if err := protojson.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode ExportConfigResponse: %v", err)
	}
	return res.GetYaml()
}
