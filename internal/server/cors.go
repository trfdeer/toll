package server

import "net/http"

// cors wraps next with permissive CORS: every origin is allowed, along with
// the methods and headers the gateway's clients use. It also answers
// preflight (OPTIONS) requests directly.
//
// The gateway authenticates with bearer tokens rather than cookies, so the
// wildcard origin is safe to use here; there is no ambient credential a
// cross-site caller could ride on.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		// Echo whatever the client asks for, falling back to a broad default
		// when there is no preflight to read from.
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			h.Set("Access-Control-Allow-Headers", req)
		} else {
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Origin, X-Requested-With")
		}
		h.Set("Access-Control-Expose-Headers", "*")
		h.Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
