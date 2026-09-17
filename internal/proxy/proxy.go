// Package proxy implements the generic OpenAI-compatible reverse proxy.
//
// Design rules (per HYPERGW-2):
//   - Streaming passthrough: responses are flushed to the client immediately,
//     so SSE chunks arrive as the upstream emits them.
//   - Error passthrough: upstream status codes and bodies are forwarded
//     verbatim — if it fails, it fails; no retry, no failover.
//   - The client's virtual key is replaced with the upstream API key; the
//     upstream credential never reaches the client.
package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/log"
)

// sharedTransport is the hardened transport for upstream calls: bounded dial
// and response-header times (a hung upstream must not pin a gateway slot
// forever), pooled keep-alive connections. No overall deadline — streams can
// run long; the response must simply start.
func sharedTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Minute,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
	}
}

// New returns the proxy handler for a single upstream.
//
// base is the upstream API root including any path prefix, e.g.
// https://api.example.com/v1. Client requests to the gateway's /v1/* are
// mapped onto that root.
func New(base *url.URL, apiKey string, logger *log.Logger) http.Handler {
	rp := &httputil.ReverseProxy{
		// FlushInterval < 0 flushes immediately after each write, which is
		// what keeps SSE streams live through the proxy.
		FlushInterval: -1,
		Transport:     sharedTransport(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out

			// Map /v1/<rest> onto <base>/<rest>.
			out.URL.Path = joinPaths(base.Path, stripV1(out.URL.Path))
			out.URL.RawPath = ""

			out.URL.Scheme = base.Scheme
			out.URL.Host = base.Host
			out.Host = base.Host

			// Replace client credentials with the upstream key.
			out.Header.Set("Authorization", "Bearer "+apiKey)

			// Identity of our client, not the gateway's IP.
			pr.SetXForwarded()
		},
		// Any transport-level failure (DNS, connect, TLS, abort) becomes a
		// 502 with a small body; everything the upstream actually sent is
		// passed through untouched.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("upstream request failed", "err", err, "path", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream unavailable","type":"gateway_error"}}`))
		},
	}

	return rp
}

// stripV1 removes a leading "/v1" (with or without trailing slash) from the
// gateway request path.
func stripV1(p string) string {
	p = strings.TrimPrefix(p, "/v1")
	if p == "" {
		return "/"
	}
	return p
}

// joinPaths merges a base path with a relative suffix, keeping exactly one
// slash between them.
func joinPaths(base, rest string) string {
	base = strings.TrimSuffix(base, "/")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if base == "" {
			return "/"
		}
		return base
	}
	if base == "" {
		return "/" + rest
	}
	return base + "/" + rest
}
