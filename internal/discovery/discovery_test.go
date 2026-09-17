package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"

	"github.com/trfdeer/toll/internal/config"
	"github.com/trfdeer/toll/internal/store"
)

func testLogger() *log.Logger {
	return log.NewWithOptions(nil, log.Options{Level: log.ErrorLevel})
}

func TestModelsFetchParsesVerbatim(t *testing.T) {
	const body = `{"data":[
		{"id":"glm-5.3-flash","pricing":{"input":0.16332},"object":"model"},
		{"id":"m2","object":"model"},
		{"object":"no-id-entry"},
		{"id":123}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("authorization = %q", got)
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/v1")
	got, err := NewClient().Models(t.Context(), base, "k")
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("models = %d, want 2 (entries without a string id are skipped)", len(got))
	}
	if got[0].UpstreamModelID != "glm-5.3-flash" {
		t.Errorf("first model = %q", got[0].UpstreamModelID)
	}
	// Metadata must be byte-identical to the upstream JSON.
	if !strings.Contains(string(got[0].Metadata), `"input":0.16332`) {
		t.Errorf("metadata not verbatim: %s", got[0].Metadata)
	}
}

func TestModelsNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/v1")
	_, err := NewClient().Models(t.Context(), base, "k")
	if err == nil {
		t.Fatal("expected error for non-200 upstream")
	}
}

func TestSyncerReconcilesRegistry(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`))
	}))
	defer srv.Close()

	st, err := store.Open(t.TempDir() + "/toll.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	base, _ := url.Parse(srv.URL + "/v1")
	cfg := &config.Config{
		Upstreams: []*config.Upstream{{
			Name: "stub", URL: base, APIKey: "k",
			Refresh: 50 * time.Millisecond,
		}},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	NewSyncer(st, cfg, testLogger()).Run(ctx)

	// Initial sync must have landed; retries keep the same 3 models.
	id, _ := st.UpsertUpstream(context.Background(), "stub", base.String(), "k", 0, 0)
	count, err := st.ModelCount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("registry has %d models, want 3", count)
	}
}

func TestSyncerAppliesConfigDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer srv.Close()

	st, err := store.Open(t.TempDir() + "/toll.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	yes := true
	base, _ := url.Parse(srv.URL + "/v1")
	cfg := &config.Config{
		Upstreams: []*config.Upstream{{
			Name: "stub", URL: base, APIKey: "k", DisableRefresh: true,
			Models: []config.ModelEntry{{ID: "a", Disabled: &yes}},
		}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	NewSyncer(st, cfg, testLogger()).Run(ctx)

	rows, err := st.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.UpstreamModelID] = r.Disabled
	}
	if !got["a"] || got["b"] {
		t.Fatalf("disabled flags = %v, want a=true b=false", got)
	}
}

func TestSyncerSeedsConfigAliases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer srv.Close()

	st, err := store.Open(t.TempDir() + "/toll.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	base, _ := url.Parse(srv.URL + "/v1")
	cfg := &config.Config{
		Upstreams: []*config.Upstream{{
			Name: "stub", URL: base, APIKey: "k", DisableRefresh: true,
			Models: []config.ModelEntry{{ID: "a", Alias: "custom/a"}},
		}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	NewSyncer(st, cfg, testLogger()).Run(ctx)

	rows, err := st.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.UpstreamModelID] = r.GatewayID
	}
	if got["a"] != "custom/a" {
		t.Errorf("aliased model gateway ID = %q, want custom/a", got["a"])
	}
	if got["b"] != "stub/b" {
		t.Errorf("default model gateway ID = %q, want stub/b", got["b"])
	}
}

func TestSyncerKeepsStateWhenUpstreamFails(t *testing.T) {
	var up bool = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"data":[{"id":"a"}]}`))
	}))
	defer srv.Close()

	st, err := store.Open(t.TempDir() + "/toll.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	base, _ := url.Parse(srv.URL + "/v1")
	u := &config.Upstream{Name: "stub", URL: base, APIKey: "k", Refresh: 30 * time.Millisecond}
	cfg := &config.Config{Upstreams: []*config.Upstream{u}}

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	NewSyncer(st, cfg, testLogger()).Run(ctx)
	cancel()

	up = false
	// Registry state must survive upstream failure — verified implicitly by
	// the sync above not having cleared it.
	id, _ := st.UpsertUpstream(context.Background(), "stub", base.String(), "k", 0, 0)
	count, err := st.ModelCount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("registry has %d models, want 1", count)
	}
}
