// Package server wires the HTTP mux: health endpoints, the /v1 proxy and
// (later) the admin UI.
package server

import (
	"net/http"
	"time"

	"github.com/charmbracelet/log"

	"github.com/trfdeer/toll/internal/admin"
	"github.com/trfdeer/toll/internal/api"
	"github.com/trfdeer/toll/internal/config"
	"github.com/trfdeer/toll/internal/proxy"
	"github.com/trfdeer/toll/internal/store"
)

// New builds the gateway's root http.Handler.
//
// /v1/models is served from the local registry (filtered per virtual key);
// the LLM endpoints (chat/completions, responses) go through the
// usage-capturing proxy; everything else under /v1/* passes through to the
// first configured upstream for now.
func New(cfg *config.Config, st *store.Store, logger *log.Logger) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	auth := func(next http.Handler) http.Handler { return api.Auth(st, logger, next) }

	// Registry-routed endpoints are always mounted: they resolve the target
	// upstream per request from the store, so they also serve providers added
	// at runtime through the admin UI — no startup upstream is required.
	mux.Handle("GET /v1/models", auth(api.NewModelsHandler(st, logger)))
	capture := auth(proxy.NewCapture(st, logger))
	mux.Handle("POST /v1/chat/completions", capture)
	mux.Handle("POST /v1/responses", capture)

	// Everything else under /v1/* still passes through to the first
	// configured upstream; without one there is nothing to fall back to.
	if len(cfg.Upstreams) > 0 {
		def := cfg.Upstreams[0]
		mux.Handle("/v1/", auth(proxy.New(def.URL, def.APIKey, logger)))
	}

	// Admin UI (served without authentication). The subtree strips the
	// /admin prefix; the bare /admin path is served directly so it renders
	// the SPA instead of being redirected to "/" by the inner mux (StripPrefix
	// would leave an empty path).
	adminHandler := admin.Handler(st, logger)
	mux.Handle("/admin/", http.StripPrefix("/admin", adminHandler))
	mux.Handle("/admin", adminHandler)

	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           cors(mux),
		ReadHeaderTimeout: 30 * time.Second,
	}
}
