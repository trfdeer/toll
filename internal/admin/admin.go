// Package admin serves the admin web UI and its JSON API under /admin.
//
// The UI is a React SPA (built with Vite from web/, see web/README.md);
// the built bundle is embedded and served from web/dist. Data endpoints
// live under /admin/api/*; everything else serves the SPA (client-side
// routing falls back to index.html).
//
// The /admin tree is served without authentication; bind it to a trusted
// interface (or put it behind your own auth proxy) when exposing the
// gateway.
package admin

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/charmbracelet/log"

	adminv1connect "github.com/trfdeer/toll/gen/toll/admin/v1/adminv1connect"
	"github.com/trfdeer/toll/internal/store"
)

//go:embed all:web/dist
var distFS embed.FS

// Handler builds the /admin subrouter.
func Handler(st *store.Store, logger *log.Logger) http.Handler {
	h := &handlers{store: st, logger: logger}

	mux := http.NewServeMux()
	// The ConnectRPC surface (MIGRATION.md): mounted alongside the legacy
	// REST routes; resources migrate one by one and unmigrated RPCs answer
	// CodeUnimplemented. POST-only: unary Connect requests, and the methodless
	// prefix would conflict with the SPA's GET catchall below. The handler
	// matches canonical procedure paths, so the /api mount prefix is stripped.
	_, connectHandler := adminv1connect.NewAdminServiceHandler(&connectService{handlers: h})
	mux.Handle("POST /api/"+adminv1connect.AdminServiceName+"/", http.StripPrefix("/api", connectHandler))
	mux.HandleFunc("GET /{$}", h.spa)
	mux.HandleFunc("GET /{rest...}", h.spa)

	return mux
}

type handlers struct {
	store  *store.Store
	logger *log.Logger
}

// fail logs the cause and answers 500 with a generic message, so store
// internals never reach the client.
func (h *handlers) fail(w http.ResponseWriter, err error, msg string) {
	h.logger.Error("admin api failed", "err", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"error":` + strconv.Quote(msg) + `}`))
}

// ---- SPA ----

// spa serves the embedded Vite build, falling back to index.html for
// client-side routes (any GET that is not a real file).
func (h *handlers) spa(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		h.fail(w, err, "admin ui unavailable")
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	if info, err := fs.Stat(sub, name); err != nil || info.IsDir() {
		name = "index.html"
	}
	http.ServeFileFS(w, r, sub, name)
}
