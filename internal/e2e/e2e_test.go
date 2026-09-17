// Package e2e spins up the entire gateway stack — config, store, discovery,
// admin, capture proxy — against a scripted stub upstream and exercises it
// over real HTTP, exactly as a client would.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"

	"github.com/trfdeer/toll/internal/config"
	"github.com/trfdeer/toll/internal/discovery"
	"github.com/trfdeer/toll/internal/keys"
	"github.com/trfdeer/toll/internal/server"
	"github.com/trfdeer/toll/internal/store"
)

// stubUpstream is a scripted OpenAI-compatible upstream whose behavior each
// test controls.
type stubUpstream struct {
	srv *httptest.Server

	mu      sync.Mutex
	models  string // /v1/models body
	chat    func(w http.ResponseWriter, r *http.Request)
	respons func(w http.ResponseWriter, r *http.Request)
}

func newStub(t *testing.T) *stubUpstream {
	s := &stubUpstream{models: `{"data":[{"id":"glm-5.3-flash","pricing":{"input":0.16332,"output":0.5444,"cache_hit":0.0315752},"context_window":202000}]}`}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Write([]byte(s.models))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		chat := s.chat
		s.mu.Unlock()
		if chat != nil {
			chat(w, r)
			return
		}
		w.WriteHeader(http.StatusNotImplemented)
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		respons := s.respons
		s.mu.Unlock()
		if respons != nil {
			respons(w, r)
			return
		}
		w.WriteHeader(http.StatusNotImplemented)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

type gateway struct {
	st  *store.Store
	srv *httptest.Server
	key string
	cfg *config.Config
}

func startGateway(t *testing.T, stub *stubUpstream, provider, model store.KeyFilter) *gateway {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	base := stub.srv.URL + "/v1"
	u, _ := url.Parse(base)
	up := &config.Upstream{
		Name: "stub", URL: u, APIKey: "upstream-secret", Refresh: time.Hour,
		AliasRules: []config.AliasRule{{Match: "^(.+)$", As: "stub/$1", Name: "Stub $1"}},
	}
	if err := up.Compile(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen:          "127.0.0.1:0",
		ShutdownTimeout: 5 * time.Second,
		Upstreams:       []*config.Upstream{up},
	}
	logger := log.NewWithOptions(nil, log.Options{Level: log.ErrorLevel})

	// Discovery: one sync, as at startup.
	syncCtx, cancelSync := context.WithCancel(t.Context())
	go discovery.NewSyncer(st, cfg, logger).Run(syncCtx)
	waitFor(t, "model discovery", func() bool {
		n, err := st.ModelCount(t.Context(), 1)
		return err == nil && n > 0
	})

	plaintext, hash, _ := keys.Generate()
	if _, err := st.CreateVirtualKey(t.Context(), "app", hash, provider, model); err != nil {
		t.Fatal(err)
	}

	srv := server.New(cfg, st, logger)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	t.Cleanup(cancelSync)

	return &gateway{st: st, srv: ts, key: plaintext, cfg: cfg}
}

func (g *gateway) get(t *testing.T, path, key string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", g.srv.URL+path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := new(strings.Builder)
	_, _ = io.Copy(body, resp.Body)
	return resp, body.String()
}

func (g *gateway) post(t *testing.T, path, key, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", g.srv.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := new(strings.Builder)
	_, _ = io.Copy(out, resp.Body)
	return resp, out.String()
}

// waitFor polls cond until true or the deadline, failing the test on timeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func parseModels(t *testing.T, body string) []map[string]any {
	t.Helper()
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("bad /v1/models body: %v: %s", err, body)
	}
	return list.Data
}
