package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/log"

	"github.com/trfdeer/toll/internal/config"
	"github.com/trfdeer/toll/internal/store"
)

// TestAdminBarePathServesSPA guards against a regression where /admin was
// 307-redirected to "/": StripPrefix leaves an empty path, which the admin
// mux cleans back to "/", so the bare path must be mounted without stripping.
func TestAdminBarePathServesSPA(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "toll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{Listen: "127.0.0.1:0"}
	logger := log.NewWithOptions(nil, log.Options{Level: log.ErrorLevel})
	h := New(cfg, st, logger).Handler

	for _, path := range []string{"/admin", "/admin/", "/admin/providers"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Errorf("GET %s did not serve the SPA: %s", path, rec.Body.String())
		}
	}

	// The API still resolves through the stripped subtree.
	req := httptest.NewRequest("GET", "/admin/api/providers", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /admin/api/providers = %d, want 200", rec.Code)
	}
}
