// Package api implements the gateway's own endpoints: authenticated model
// listing and the auth middleware shared with proxied routes.
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/charmbracelet/log"

	"github.com/trfdeer/toll/internal/keys"
	"github.com/trfdeer/toll/internal/store"
)

type ctxKey int

const vkKey ctxKey = 1

// VirtualKeyFrom returns the authenticated key on the request context.
func VirtualKeyFrom(ctx context.Context) *keys.VirtualKey {
	vk, _ := ctx.Value(vkKey).(*keys.VirtualKey)
	return vk
}

// Auth wraps next with virtual-key authentication. Upstream key material
// never leaves the gateway; only hashed virtual keys are consulted.
func Auth(st *store.Store, logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := keys.ParseBearer(r.Header.Get("Authorization"))
		if !ok {
			unauthorized(w)
			return
		}
		vk, err := st.KeyByHash(r.Context(), keys.Hash(token))
		if err != nil {
			logger.Warn("authentication failed", "err", err, "path", r.URL.Path)
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), vkKey, toKeysKey(vk))))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"invalid API key","type":"invalid_request_error"}}`))
}

// toKeysKey converts the store row into the keys-package type used on the
// request context (they are intentionally separate types; this is the one
// bridge point).
func toKeysKey(vk *store.VirtualKey) *keys.VirtualKey {
	return &keys.VirtualKey{
		ID:             vk.ID,
		Name:           vk.Name,
		ProviderFilter: toKeysFilter(vk.ProviderFilter),
		ModelFilter:    toKeysFilter(vk.ModelFilter),
	}
}

func toKeysFilter(f store.KeyFilter) keys.Filter {
	return keys.Filter{Mode: keys.FilterMode(f.Mode), Values: f.Values}
}

// NewModelsHandler serves GET /v1/models: the merged cross-upstream model
// list, filtered by the virtual key's filters and excluding disabled models.
//
// Every entry is emitted in one shape, regardless of upstream — the same
// fields hyper.charm.land/v1/models uses (id, object, created, owned_by,
// display_name, plus optional context_window, max_output_tokens,
// capabilities, reasoning and pricing). Provider-specific metadata is
// normalized into that shape rather than re-emitted verbatim.
func NewModelsHandler(st *store.Store, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vk := VirtualKeyFrom(r.Context())
		if vk == nil {
			unauthorized(w)
			return
		}

		rows, err := st.ListModels(r.Context())
		if err != nil {
			logger.Error("list models", "err", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"registry unavailable","type":"gateway_error"}}`))
			return
		}

		data := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			// A model is hidden when it is disabled, when its provider is
			// disabled, or when the provider was unreachable on the last sync
			// (so clients don't try a server we know is down).
			if row.Disabled || row.UpstreamDisabled || !row.UpstreamReachable {
				continue
			}
			// Provider is the upstream the model was discovered from, not a
			// prefix of its ID.
			if !keys.Allows(row.UpstreamName, vk.ProviderFilter, vk.ModelFilter, row.GatewayID) {
				continue
			}
			var meta map[string]any
			if err := json.Unmarshal([]byte(row.Metadata), &meta); err != nil {
				logger.Warn("skipping model with corrupt metadata", "model", row.GatewayID, "err", err)
				continue
			}
			data = append(data, unifiedModel(row, meta))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   data,
		})
	})
}

// unifiedModel renders one registry entry in the gateway's single
// OpenAI-compatible model shape. The first five keys are always present;
// the rest appear only when the merged metadata carries a usable value.
func unifiedModel(row store.ModelRow, meta map[string]any) map[string]any {
	m := map[string]any{
		"id":           row.GatewayID,
		"object":       "model",
		"created":      intField(meta, "created"),
		"owned_by":     row.UpstreamName,
		"display_name": row.DisplayName,
	}
	// Input bound: upstreams name the context window differently.
	if v, ok := firstNumber(meta,
		"context_window", "max_model_len", "context_length", "max_context_length"); ok {
		m["context_window"] = v
	}
	// Output bound: top-level names, then OpenRouter's nested top_provider.
	if v, ok := firstNumber(meta,
		"max_output_tokens", "max_completion_tokens", "max_tokens"); ok {
		m["max_output_tokens"] = v
	} else if tp, ok := meta["top_provider"].(map[string]any); ok {
		if v, ok := firstNumber(tp, "max_completion_tokens"); ok {
			m["max_output_tokens"] = v
		}
	}
	for _, k := range []string{"capabilities", "reasoning", "pricing"} {
		if v, ok := meta[k]; ok {
			m[k] = v
		}
	}
	return m
}

// firstNumber returns the first present numeric field among keys.
func firstNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if f, ok := m[k].(float64); ok {
			return f, true
		}
	}
	return 0, false
}

// intField reads a numeric field as an int64 (0 when absent).
func intField(m map[string]any, key string) int64 {
	if f, ok := m[key].(float64); ok {
		return int64(f)
	}
	return 0
}
