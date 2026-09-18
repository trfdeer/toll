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
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"

	"github.com/trfdeer/toll/internal/keys"
	"github.com/trfdeer/toll/internal/store"
)

//go:embed all:web/dist
var distFS embed.FS

// Handler builds the /admin subrouter.
func Handler(st *store.Store, logger *log.Logger) http.Handler {
	h := &handlers{store: st, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/usage", h.usage)
	mux.HandleFunc("GET /api/requests", h.requests)
	mux.HandleFunc("GET /api/requests/{id}", h.requestDetail)
	mux.HandleFunc("GET /api/providers", h.providers)
	mux.HandleFunc("POST /api/providers", h.providersCreate)
	mux.HandleFunc("POST /api/providers/{name}/disable", h.providersDisable)
	mux.HandleFunc("POST /api/providers/{name}/enable", h.providersEnable)
	mux.HandleFunc("DELETE /api/providers/{name}", h.providersDelete)
	mux.HandleFunc("GET /api/models", h.models)
	mux.HandleFunc("POST /api/models/refresh", h.modelsRefresh)
	mux.HandleFunc("DELETE /api/models/{id}", h.modelsDelete)
	mux.HandleFunc("POST /api/models/{id}/disable", h.modelsDisable)
	mux.HandleFunc("POST /api/models/{id}/enable", h.modelsEnable)
	mux.HandleFunc("PUT /api/models/{id}/alias", h.modelsSetAlias)
	mux.HandleFunc("GET /api/config", h.configExport)
	mux.HandleFunc("GET /api/settings", h.settingsGet)
	mux.HandleFunc("PUT /api/settings", h.settingsPut)
	mux.HandleFunc("GET /api/profiles", h.profilesList)
	mux.HandleFunc("POST /api/profiles", h.profilesCreate)
	mux.HandleFunc("PUT /api/profiles/{name}", h.profilesUpdate)
	mux.HandleFunc("DELETE /api/profiles/{name}", h.profilesDelete)
	mux.HandleFunc("GET /api/keys", h.keysList)
	mux.HandleFunc("POST /api/keys", h.keysCreate)
	mux.HandleFunc("PUT /api/keys/{name}", h.keysUpdate)
	mux.HandleFunc("POST /api/keys/{name}/revoke", h.keysRevoke)
	mux.HandleFunc("POST /api/keys/{name}/pause", h.keysPause)
	mux.HandleFunc("POST /api/keys/{name}/resume", h.keysResume)
	mux.HandleFunc("DELETE /api/keys/{name}", h.keysDelete)
	mux.HandleFunc("GET /{$}", h.spa)
	mux.HandleFunc("GET /{rest...}", h.spa)

	return mux
}

type handlers struct {
	store  *store.Store
	logger *log.Logger
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func readJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return v, false
	}
	return v, true
}

func (h *handlers) fail(w http.ResponseWriter, err error, msg string) {
	h.logger.Error("admin api failed", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
}

// ---- usage ----

func (h *handlers) usage(w http.ResponseWriter, r *http.Request) {
	filter, ok := parseFilter(w, r)
	if !ok {
		return
	}
	sum, err := h.store.UsageSummary(r.Context(), filter)
	if err != nil {
		h.fail(w, err, "usage unavailable")
		return
	}
	type usageRow struct {
		GatewayModel     string  `json:"gatewayModel"`
		Requests         int     `json:"requests"`
		PromptTokens     int     `json:"promptTokens"`
		CachedTokens     int     `json:"cachedTokens"`
		CompletionTokens int     `json:"completionTokens"`
		CostUSD          float64 `json:"costUSD"`
	}
	rows := make([]usageRow, 0, len(sum))
	var totalReqs int
	var total float64
	for _, s := range sum {
		rows = append(rows, usageRow{
			GatewayModel: s.GatewayModel,
			Requests:     s.Requests, PromptTokens: s.PromptTokens,
			CachedTokens: s.CachedTokens, CompletionTokens: s.CompletionToken,
			CostUSD: s.CostUSD,
		})
		totalReqs += s.Requests
		total += s.CostUSD
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":      rows,
		"totalReqs": totalReqs,
		"totalCost": fmtUSD(total),
	})
}

// requests lists requests (one per transcript), newest first, filtered by
// the optional from/to (RFC3339), key, limit and offset query parameters.
func (h *handlers) requests(w http.ResponseWriter, r *http.Request) {
	filter, ok := parseFilter(w, r)
	if !ok {
		return
	}
	limit, offset := 200, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be 1..1000"})
			return
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "offset must be >= 0"})
			return
		}
		offset = n
	}
	reqs, total, err := h.store.Requests(r.Context(), store.RequestFilter{
		UsageFilter: filter, Limit: limit, Offset: offset,
	})
	if err != nil {
		h.fail(w, err, "requests unavailable")
		return
	}
	type request struct {
		ID               int64    `json:"id"`
		ConversationID   string   `json:"conversationId"`
		KeyName          string   `json:"keyName"`
		GatewayModel     string   `json:"gatewayModel"`
		Status           int      `json:"status"`
		PromptTokens     int      `json:"promptTokens"`
		CompletionTokens int      `json:"completionTokens"`
		CachedTokens     int      `json:"cachedTokens"`
		CostUSD          *float64 `json:"costUSD"`
		CreatedAt        string   `json:"createdAt"`
		DurationMS       *int64   `json:"durationMs"`
	}
	out := make([]request, 0, len(reqs))
	for _, t := range reqs {
		out = append(out, request{
			ID: t.ID, ConversationID: t.ConversationID, KeyName: t.KeyName,
			GatewayModel: t.GatewayModel, Status: t.Status,
			PromptTokens: t.PromptTokens, CompletionTokens: t.CompletionToken,
			CachedTokens: t.CachedTokens, CostUSD: t.CostUSD, CreatedAt: t.CreatedAt,
			DurationMS: t.DurationMS,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out, "total": total})
}

// parseFilter reads the optional from/to (RFC3339) and key query parameters
// shared by the usage and request endpoints. The reserved key value
// deletedKeysSentinel selects events whose key has been deleted. It writes a
// 400 on bad input.
func parseFilter(w http.ResponseWriter, r *http.Request) (store.UsageFilter, bool) {
	q := r.URL.Query()
	f := store.UsageFilter{}
	for _, k := range q["key"] {
		if k == deletedKeysSentinel {
			f.IncludeDeleted = true
			continue
		}
		f.Keys = append(f.Keys, k)
	}
	for _, p := range []struct {
		name string
		dst  *string
	}{{"from", &f.From}, {"to", &f.To}} {
		v := q.Get(p.name)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": p.name + " must be an RFC3339 timestamp"})
			return f, false
		}
		*p.dst = store.FormatTime(t)
	}
	return f, true
}

// deletedKeysSentinel is the reserved key-filter value that selects events
// whose virtual key no longer exists. It is unlikely to collide with a real
// key name (key names are user-chosen, so this is a documented convention).
const deletedKeysSentinel = "__deleted__"

// ---- providers & models ----

func (h *handlers) providers(w http.ResponseWriter, r *http.Request) {
	ups, err := h.store.ListUpstreams(r.Context())
	if err != nil {
		h.fail(w, err, "registry unavailable")
		return
	}
	type provider struct {
		Name         string `json:"name"`
		BaseURL      string `json:"baseURL"`
		ModelCount   int    `json:"modelCount"`
		Disabled     bool   `json:"disabled"`
		Reachable    bool   `json:"reachable"`
		LastError    string `json:"lastError"`
		LastSyncedAt string `json:"lastSyncedAt"`
	}
	out := make([]provider, 0, len(ups))
	for _, u := range ups {
		out = append(out, provider{
			Name: u.Name, BaseURL: u.BaseURL, ModelCount: u.ModelCount,
			Disabled: u.Disabled, Reachable: u.Reachable,
			LastError: u.LastError, LastSyncedAt: u.LastSyncedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// providersDisable hides every model of a provider from /v1/models and
// refuses routing to them, without touching the registry.
func (h *handlers) providersDisable(w http.ResponseWriter, r *http.Request) {
	h.providersSetDisabled(w, r, true)
}

// providersEnable reverses providersDisable.
func (h *handlers) providersEnable(w http.ResponseWriter, r *http.Request) {
	h.providersSetDisabled(w, r, false)
}

func (h *handlers) providersSetDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	name := r.PathValue("name")
	if err := h.store.SetUpstreamDisabled(r.Context(), name, disabled); err != nil {
		if errors.Is(err, store.ErrUpstreamNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider not found"})
			return
		}
		h.fail(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

type createProviderRequest struct {
	Name      string `json:"name"`
	BaseURL   string `json:"baseURL"`
	APIKey    string `json:"apiKey"`
	RefreshIn int    `json:"refreshSeconds"`
}

// providersCreate registers an upstream in the registry. The upstream's
// model catalog is pulled immediately (best effort) so the provider is
// usable right away; if that fetch fails the provider is kept and the
// response carries a warning.
func (h *handlers) providersCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := readJSON[createProviderRequest](w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(req.Name)
	baseURL := strings.TrimSpace(req.BaseURL)
	if name == "" || baseURL == "" || req.APIKey == "" {
		writeJSON(w, http.StatusUnprocessableEntity,
			map[string]string{"error": "name, baseURL and apiKey are required"})
		return
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		writeJSON(w, http.StatusUnprocessableEntity,
			map[string]string{"error": "baseURL must be an http(s) URL"})
		return
	}
	refresh := req.RefreshIn
	if refresh <= 0 {
		refresh = 300
	}
	position, err := h.store.NextUpstreamPosition(r.Context())
	if err != nil {
		h.fail(w, err, "could not add provider")
		return
	}
	id, err := h.store.UpsertUpstream(r.Context(), name, baseURL, req.APIKey, refresh, position)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity,
			map[string]string{"error": "could not add provider: " + err.Error()})
		return
	}

	count, warning := h.syncProvider(r.Context(), name, baseURL, req.APIKey, id)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "baseURL": baseURL, "modelCount": count, "warning": warning,
	})
}

// providersDelete removes an upstream and its models. Models served by it
// disappear from the registry immediately.
func (h *handlers) providersDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.store.DeleteUpstream(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrUpstreamNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider not found"})
			return
		}
		h.fail(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (h *handlers) models(w http.ResponseWriter, r *http.Request) {
	ms, err := h.store.ListModels(r.Context())
	if err != nil {
		h.fail(w, err, "models unavailable")
		return
	}
	type model struct {
		ID                int64           `json:"id"`
		UpstreamName      string          `json:"upstream"`
		UpstreamModelID   string          `json:"upstreamModelId"`
		GatewayID         string          `json:"gatewayId"`
		DisplayName       string          `json:"displayName"`
		Alias             string          `json:"alias"`
		Metadata          json.RawMessage `json:"metadata"`
		Disabled          bool            `json:"disabled"`
		ProviderDisabled  bool            `json:"providerDisabled"`
		ProviderReachable bool            `json:"providerReachable"`
	}
	out := make([]model, 0, len(ms))
	for _, m := range ms {
		meta := json.RawMessage(m.Metadata)
		if !json.Valid(meta) {
			meta = json.RawMessage(`{}`)
		}
		out = append(out, model{
			ID: m.ID, UpstreamName: m.UpstreamName, UpstreamModelID: m.UpstreamModelID,
			GatewayID: m.GatewayID, DisplayName: m.DisplayName, Alias: m.Alias, Metadata: meta,
			Disabled: m.Disabled, ProviderDisabled: m.UpstreamDisabled,
			ProviderReachable: m.UpstreamReachable,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// modelsRefresh re-pulls every provider's model catalog and reconciles the
// registry, so the Models page can force a re-discovery instead of waiting
// for the periodic sync. Failures are reported per provider and do not stop
// the others.
func (h *handlers) modelsRefresh(w http.ResponseWriter, r *http.Request) {
	ups, err := h.store.ListUpstreamsForSync(r.Context())
	if err != nil {
		h.fail(w, err, "refresh failed")
		return
	}
	total := 0
	warnings := make([]string, 0)
	h.logger.Info("models refresh: starting", "providers", len(ups))
	for _, u := range ups {
		n, warning := h.syncProvider(r.Context(), u.Name, u.BaseURL, u.APIKey, u.ID)
		total += n
		if warning != "" {
			warnings = append(warnings, u.Name+": "+warning)
		}
	}
	h.logger.Info("models refresh: done", "providers", len(ups), "models", total, "warnings", len(warnings))
	for _, w := range warnings {
		h.logger.Warn("models refresh: provider warning", "warning", w)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": len(ups),
		"models":    total,
		"warnings":  warnings,
	})
}

// modelsDisable hides a model from /v1/models and refuses routing to it,
// without removing it from the registry.
func (h *handlers) modelsDisable(w http.ResponseWriter, r *http.Request) {
	h.modelsSetDisabled(w, r, true)
}

// modelsEnable reverses modelsDisable.
func (h *handlers) modelsEnable(w http.ResponseWriter, r *http.Request) {
	h.modelsSetDisabled(w, r, false)
}

func (h *handlers) modelsSetDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	if err := h.store.SetModelDisabled(r.Context(), id, disabled); err != nil {
		if errors.Is(err, store.ErrModelNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "model not found"})
			return
		}
		h.fail(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// modelsDelete removes one registry entry. Removal is local only: the next
// upstream discovery refresh may re-add a model the upstream still reports.
// Disabling (see modelsDisable) keeps it out without that risk.
func (h *handlers) modelsDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	if err := h.store.DeleteModel(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrModelNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "model not found"})
			return
		}
		h.fail(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// ---- settings ----

// modelsSetAlias sets or clears a model's custom gateway ID (alias). An empty
// alias restores the model's computed ID. The change applies immediately and
// survives later discovery syncs.
func (h *handlers) modelsSetAlias(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	req, ok := readJSON[struct {
		Alias string `json:"alias"`
	}](w, r)
	if !ok {
		return
	}
	if err := h.store.SetModelAlias(r.Context(), id, req.Alias); err != nil {
		switch {
		case errors.Is(err, store.ErrModelNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "model not found"})
		case errors.Is(err, store.ErrAliasConflict):
			writeJSON(w, http.StatusUnprocessableEntity,
				map[string]string{"error": "alias is already another model's gateway ID"})
		default:
			h.fail(w, err, "update failed")
		}
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// settingsView is the admin-editable runtime settings surface.
type settingsView struct {
	StorePrompts bool `json:"storePrompts"`
}

func (h *handlers) settingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, settingsView{StorePrompts: h.store.PromptsEnabled()})
}

type updateSettingsRequest struct {
	StorePrompts *bool `json:"storePrompts"`
}

// settingsPut updates the runtime settings. Only prompt storage is settable
// today; omitted fields are left unchanged.
func (h *handlers) settingsPut(w http.ResponseWriter, r *http.Request) {
	req, ok := readJSON[updateSettingsRequest](w, r)
	if !ok {
		return
	}
	if req.StorePrompts != nil {
		if err := h.store.SetSetting(r.Context(), store.SettingStorePrompts,
			strconv.FormatBool(*req.StorePrompts)); err != nil {
			h.fail(w, err, "could not save settings")
			return
		}
	}
	writeJSON(w, http.StatusOK, settingsView{StorePrompts: h.store.PromptsEnabled()})
}

// ---- profiles ----

type profileView struct {
	Name           string          `json:"name"`
	ProviderFilter store.KeyFilter `json:"providerFilter"`
	ModelFilter    store.KeyFilter `json:"modelFilter"`
	Parents        []string        `json:"parents"`
	IsDefault      bool            `json:"isDefault"`
	KeyCount       int             `json:"keyCount"`
	ChildCount     int             `json:"childCount"`
}

func (h *handlers) profilesList(w http.ResponseWriter, r *http.Request) {
	ps, err := h.store.ListProfiles(r.Context())
	if err != nil {
		h.fail(w, err, "profiles unavailable")
		return
	}
	out := make([]profileView, 0, len(ps))
	for _, p := range ps {
		parents := p.Parents
		if parents == nil {
			parents = []string{}
		}
		out = append(out, profileView{
			Name: p.Name, ProviderFilter: p.ProviderFilter, ModelFilter: p.ModelFilter,
			Parents: parents, IsDefault: p.IsDefault,
			KeyCount: p.KeyCount, ChildCount: p.ChildCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

type profileRequest struct {
	Name           string          `json:"name"`
	ProviderFilter store.KeyFilter `json:"providerFilter"`
	ModelFilter    store.KeyFilter `json:"modelFilter"`
	Parents        []string        `json:"parents"`
}

// validateProfileRequest checks the name, both filters and the leaf/derived
// XOR: a profile either carries filters or references parents, never both. It
// trims parent names in place so the store sees the same values that were
// validated.
func validateProfileRequest(req *profileRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return errors.New("name is required")
	}
	for _, f := range []struct {
		label  string
		filter store.KeyFilter
	}{{"provider", req.ProviderFilter}, {"model", req.ModelFilter}} {
		if err := validateFilter(f.filter); err != nil {
			return errors.New(f.label + " filter: " + err.Error())
		}
	}
	seen := make(map[string]bool, len(req.Parents))
	cleaned := make([]string, 0, len(req.Parents))
	for _, parent := range req.Parents {
		parent = strings.TrimSpace(parent)
		switch {
		case parent == "":
			return errors.New("parent name is required")
		case parent == strings.TrimSpace(req.Name):
			return errors.New("a profile cannot be its own parent")
		case seen[parent]:
			return errors.New("duplicate parent " + parent)
		}
		seen[parent] = true
		cleaned = append(cleaned, parent)
	}
	req.Parents = cleaned
	if len(req.Parents) > 0 &&
		(req.ProviderFilter.Mode == "include" || req.ProviderFilter.Mode == "exclude" ||
			req.ModelFilter.Mode == "include" || req.ModelFilter.Mode == "exclude") {
		return errors.New("a derived profile has no filters of its own")
	}
	return nil
}

func (h *handlers) profilesCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := readJSON[profileRequest](w, r)
	if !ok {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := validateProfileRequest(&req); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if _, err := h.store.CreateProfile(r.Context(), req.Name, req.ProviderFilter, req.ModelFilter, req.Parents); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "could not create profile: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (h *handlers) profilesUpdate(w http.ResponseWriter, r *http.Request) {
	current := r.PathValue("name")
	req, ok := readJSON[profileRequest](w, r)
	if !ok {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := validateProfileRequest(&req); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if err := h.store.UpdateProfile(r.Context(), current, req.Name, req.ProviderFilter, req.ModelFilter, req.Parents); err != nil {
		h.writeProfileError(w, err, "could not update profile")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (h *handlers) profilesDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.store.DeleteProfile(r.Context(), name); err != nil {
		h.writeProfileError(w, err, "could not delete profile")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// writeProfileError maps the store's profile sentinels to HTTP responses.
func (h *handlers) writeProfileError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, store.ErrProfileNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "profile not found"})
	case errors.Is(err, store.ErrProfileImmutable):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "the All profile is read-only"})
	case errors.Is(err, store.ErrProfileInUse):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrProfileInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	default:
		h.fail(w, err, fallback)
	}
}

// ---- virtual keys ----

type keyView struct {
	Name    string `json:"name"`
	Profile string `json:"profile"`
	Revoked bool   `json:"revoked"`
	Paused  bool   `json:"paused"`
}

func (h *handlers) keysList(w http.ResponseWriter, r *http.Request) {
	keys, err := h.keysFor(r.Context())
	if err != nil {
		h.fail(w, err, "keys unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (h *handlers) keysFor(ctx context.Context) ([]keyView, error) {
	vks, err := h.store.ListVirtualKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]keyView, 0, len(vks))
	for _, vk := range vks {
		out = append(out, keyView{
			Name:    vk.Name,
			Profile: vk.ProfileName,
			Revoked: vk.Revoked,
			Paused:  vk.Paused,
		})
	}
	return out, nil
}

type createKeyRequest struct {
	Name    string `json:"name"`
	Profile string `json:"profile"`
}

// resolveProfileName maps an optional profile name to its id, defaulting to
// the seeded "All" profile when the request omits one.
func (h *handlers) resolveProfileName(ctx context.Context, name string) (int64, error) {
	if strings.TrimSpace(name) == "" {
		name = "All"
	}
	p, err := h.store.ProfileByName(ctx, name)
	if err != nil {
		return 0, err
	}
	return p.ID, nil
}

func (h *handlers) keysCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := readJSON[createKeyRequest](w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "name is required"})
		return
	}
	profileID, err := h.resolveProfileName(r.Context(), req.Profile)
	if err != nil {
		if errors.Is(err, store.ErrProfileNotFound) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "profile not found"})
			return
		}
		h.fail(w, err, "could not create key")
		return
	}

	plaintext, hash, err := keys.Generate()
	if err != nil {
		h.fail(w, err, "key generation failed")
		return
	}
	if _, err := h.store.CreateVirtualKey(r.Context(), name, hash, profileID); err != nil {
		// Likely a duplicate name.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "could not create key: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"plaintext": plaintext})
}

// keysUpdate edits an existing key's name and/or profile in place. The key
// material is unchanged, so clients keep working across the edit.
func (h *handlers) keysUpdate(w http.ResponseWriter, r *http.Request) {
	current := r.PathValue("name")
	req, ok := readJSON[createKeyRequest](w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "name is required"})
		return
	}
	profileID, err := h.resolveProfileName(r.Context(), req.Profile)
	if err != nil {
		if errors.Is(err, store.ErrProfileNotFound) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "profile not found"})
			return
		}
		h.fail(w, err, "could not update key")
		return
	}
	if err := h.store.UpdateVirtualKey(r.Context(), current, name, profileID); err != nil {
		if errors.Is(err, store.ErrKeyNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found"})
			return
		}
		// Most likely a rename onto an existing name.
		writeJSON(w, http.StatusUnprocessableEntity,
			map[string]string{"error": "could not update key: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// validateFilter rejects an unknown mode or an include/exclude filter with no
// values (which would allow nothing and is almost certainly a mistake).
func validateFilter(f store.KeyFilter) error {
	switch f.Mode {
	case "", "none":
		return nil
	case "include", "exclude":
		if len(f.Values) == 0 {
			return errors.New("mode " + f.Mode + " requires at least one value")
		}
		return nil
	default:
		return errors.New("mode must be none, include or exclude")
	}
}

func (h *handlers) keysRevoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.store.RevokeVirtualKey(r.Context(), name); err != nil {
		h.fail(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (h *handlers) keysPause(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.store.PauseVirtualKey(r.Context(), name); err != nil {
		h.fail(w, err, "pause failed")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (h *handlers) keysResume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.store.ResumeVirtualKey(r.Context(), name); err != nil {
		h.fail(w, err, "resume failed")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (h *handlers) keysDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.store.DeleteVirtualKey(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrKeyNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found"})
			return
		}
		h.fail(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
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

func fmtUSD(v float64) string {
	b, _ := json.Marshal(v)
	return string(b) + " USD"
}
