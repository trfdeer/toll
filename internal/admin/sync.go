package admin

import (
	"context"
	"net/url"
	"time"

	"github.com/trfdeer/toll/internal/config"
	"github.com/trfdeer/toll/internal/discovery"
	"github.com/trfdeer/toll/internal/registry"
	"github.com/trfdeer/toll/internal/store"
)

// providerSyncTimeout bounds the best-effort model pull performed when a
// provider is added through the admin API.
const providerSyncTimeout = 20 * time.Second

// syncProvider pulls one upstream's model catalog and reconciles its registry
// entries. The upstream is not part of the file config, so no alias rules or
// overlays apply: gateway IDs map one-to-one from the upstream model IDs.
// Returns the number of synced models and a warning message on failure.
func (h *handlers) syncProvider(ctx context.Context, name, baseURL, apiKey string, upstreamID int64) (int, string) {
	u, err := url.Parse(baseURL)
	if err != nil {
		h.logger.Error("provider model sync: invalid base URL", "provider", name, "err", err)
		return 0, "model sync skipped: " + err.Error()
	}

	ctx, cancel := context.WithTimeout(ctx, providerSyncTimeout)
	defer cancel()

	h.logger.Debug("provider model sync: starting", "provider", name, "base_url", u.Redacted(), "upstream_id", upstreamID)
	discovered, err := discovery.NewClient(h.logger).Models(ctx, u, apiKey)
	if err != nil {
		h.logger.Error("provider model sync: catalog fetch failed", "provider", name, "base_url", u.Redacted(), "err", err)
		h.markProviderReachable(name, false, err.Error())
		return 0, "models could not be fetched: " + err.Error()
	}
	h.logger.Debug("provider model sync: catalog discovered", "provider", name, "models", len(discovered))

	engine := registry.NewEngine(&config.Upstream{Name: name})
	models := make([]store.DiscoveredModel, 0, len(discovered))
	unresolved := 0
	for _, d := range discovered {
		r, err := engine.Resolve(d.UpstreamModelID, d.Metadata)
		if err != nil {
			unresolved++
			h.logger.Warn("provider model sync: skipping model with unresolvable metadata",
				"provider", name, "model", d.UpstreamModelID, "err", err)
			continue
		}
		models = append(models, store.DiscoveredModel{
			UpstreamModelID: d.UpstreamModelID,
			GatewayID:       r.GatewayID,
			DisplayName:     r.DisplayName,
			Metadata:        r.Metadata,
		})
	}
	if unresolved > 0 {
		h.logger.Warn("provider model sync: some models could not be resolved",
			"provider", name, "skipped", unresolved, "resolved", len(models))
	}

	skipped, err := h.store.ReplaceModels(ctx, upstreamID, models)
	if err != nil {
		h.logger.Error("provider registry sync failed", "provider", name, "err", err)
		h.markProviderReachable(name, false, err.Error())
		return 0, "models could not be stored: " + err.Error()
	}
	if skipped > 0 {
		h.logger.Warn("provider model sync: gateway ID collisions, entries skipped",
			"provider", name, "skipped", skipped, "reported", len(models))
	}
	stored := len(models) - skipped
	h.markProviderReachable(name, true, "")
	h.logger.Info("provider model sync: done",
		"provider", name, "discovered", len(discovered), "mapped", len(models), "registered", stored)
	return stored, ""
}

// markProviderReachable records a sync outcome on a detached context, so a
// timed-out fetch context doesn't drop the status update.
func (h *handlers) markProviderReachable(name string, reachable bool, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.store.SetUpstreamReachable(ctx, name, reachable, msg); err != nil {
		h.logger.Error("mark provider reachability", "provider", name, "err", err)
	}
}
