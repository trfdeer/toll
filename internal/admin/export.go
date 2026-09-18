package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/trfdeer/toll/internal/store"
)

// configExport is the YAML shape written by GET /admin/api/config. It mirrors
// the file-config surface (internal/config) so the output round-trips through
// `toll --config`: every upstream is pinned as explicit model entries because
// the registry stores resolved IDs and merged metadata, not the original alias
// rules and overlays.
type configExport struct {
	// StorePrompts is emitted only when prompt storage is disabled, so the
	// exported file reproduces the current setting.
	StorePrompts *bool            `yaml:"store_prompts,omitempty"`
	Profiles     []exportProfile  `yaml:"profiles,omitempty"`
	Upstreams    []exportUpstream `yaml:"upstreams"`
}

// exportProfile mirrors config.Profile so the emitted YAML seeds profiles on
// import. The seeded "All" default is never exported.
type exportProfile struct {
	Name           string       `yaml:"name"`
	ProviderFilter exportFilter `yaml:"provider_filter"`
	ModelFilter    exportFilter `yaml:"model_filter"`
}

// exportFilter mirrors config.ProfileFilter. Empty values are omitted; the
// mode is always written so the rule round-trips explicitly.
type exportFilter struct {
	Mode   string   `yaml:"mode"`
	Values []string `yaml:"values,omitempty"`
}

type exportUpstream struct {
	Name           string        `yaml:"name"`
	URL            string        `yaml:"url"`
	APIKeyEnv      string        `yaml:"api_key_env"`
	Refresh        string        `yaml:"refresh_interval,omitempty"`
	DisableRefresh bool          `yaml:"disable_refresh,omitempty"`
	Models         []exportModel `yaml:"models,omitempty"`
}

// exportModel matches config.ModelEntry so the emitted YAML parses as a file
// config.
type exportModel struct {
	ID       string         `yaml:"id"`
	Alias    string         `yaml:"alias,omitempty"`
	Name     string         `yaml:"name,omitempty"`
	Metadata map[string]any `yaml:"metadata,omitempty"`
	Disabled bool           `yaml:"disabled,omitempty"`
}

// configExport writes the current registry state as a toll.yaml. Secrets are
// never emitted: each upstream's key is redacted to an api_key_env reference
// derived from its name.
func (h *handlers) configExport(w http.ResponseWriter, r *http.Request) {
	data, err := exportConfig(r.Context(), h.store)
	if err != nil {
		h.fail(w, err, "config export unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="toll.yaml"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// exportConfig builds the YAML config from the store's upstream and model rows.
func exportConfig(ctx context.Context, st *store.Store) ([]byte, error) {
	ups, err := st.ListUpstreams(ctx)
	if err != nil {
		return nil, err
	}
	models, err := st.ListModelsForExport(ctx)
	if err != nil {
		return nil, err
	}
	profiles, err := st.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}

	byUpstream := make(map[int64][]store.ExportModel, len(ups))
	for _, m := range models {
		byUpstream[m.UpstreamID] = append(byUpstream[m.UpstreamID], m)
	}

	out := configExport{
		Upstreams: make([]exportUpstream, 0, len(ups)),
		Profiles:  exportProfiles(profiles),
	}
	if !st.PromptsEnabled() {
		disabled := false
		out.StorePrompts = &disabled
	}
	for _, u := range ups {
		eu := exportUpstream{
			Name:      u.Name,
			URL:       u.BaseURL,
			APIKeyEnv: apiKeyEnvName(u.Name),
			Models:    exportModels(byUpstream[u.ID]),
		}
		if u.RefreshSeconds > 0 {
			eu.Refresh = (time.Duration(u.RefreshSeconds) * time.Second).String()
		}
		out.Upstreams = append(out.Upstreams, eu)
	}
	return yaml.Marshal(out)
}

// exportProfiles converts store profiles into config entries. The read-only
// "All" default is omitted: the migration recreates it on import.
func exportProfiles(rows []store.Profile) []exportProfile {
	out := make([]exportProfile, 0, len(rows))
	for _, p := range rows {
		if p.IsDefault {
			continue
		}
		out = append(out, exportProfile{
			Name:           p.Name,
			ProviderFilter: exportFilter{Mode: p.ProviderFilter.Mode, Values: p.ProviderFilter.Values},
			ModelFilter:    exportFilter{Mode: p.ModelFilter.Mode, Values: p.ModelFilter.Values},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// exportModels converts registry rows into explicit config model entries. A
// custom alias is emitted verbatim; otherwise a computed gateway ID that
// differs from the upstream ID (an alias rule) is emitted so the resolution
// round-trips. Metadata is carried over minus the id, which the entry pins.
func exportModels(rows []store.ExportModel) []exportModel {
	if len(rows) == 0 {
		return nil
	}
	out := make([]exportModel, 0, len(rows))
	for _, m := range rows {
		em := exportModel{ID: m.UpstreamModelID, Name: m.DisplayName, Disabled: m.Disabled}
		switch {
		case m.Alias != "":
			em.Alias = m.Alias
		case m.GatewayID != m.UpstreamModelID:
			em.Alias = m.GatewayID
		}
		if m.Metadata != "" {
			var meta map[string]any
			if err := json.Unmarshal([]byte(m.Metadata), &meta); err == nil {
				delete(meta, "id")
				if len(meta) > 0 {
					em.Metadata = meta
				}
			}
		}
		out = append(out, em)
	}
	return out
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// apiKeyEnvName derives the environment variable an upstream's secret is read
// from: the upstream name upper-cased with non-alphanumerics collapsed to
// underscores, suffixed with _API_KEY.
func apiKeyEnvName(name string) string {
	s := strings.Trim(nonAlnum.ReplaceAllString(strings.ToUpper(name), "_"), "_")
	if s == "" {
		return "TOLL_UPSTREAM_API_KEY"
	}
	return s + "_API_KEY"
}
