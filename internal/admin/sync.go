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
		return 0, "model sync skipped: " + err.Error()
	}

	ctx, cancel := context.WithTimeout(ctx, providerSyncTimeout)
	defer cancel()

	discovered, err := discovery.NewClient().Models(ctx, u, apiKey)
	if err != nil {
		h.logger.Warn("provider model sync failed", "provider", name, "err", err)
		h.markProviderReachable(name, false, err.Error())
		return 0, "models could not be fetched: " + err.Error()
	}

	engine := registry.NewEngine(&config.Upstream{Name: name})
	models := make([]store.DiscoveredModel, 0, len(discovered))
	for _, d := range discovered {
		r, err := engine.Resolve(d.UpstreamModelID, d.Metadata)
		if err != nil {
			h.logger.Warn("skipping model with unresolvable metadata", "provider", name, "err", err)
			continue
		}
		models = append(models, store.DiscoveredModel{
			UpstreamModelID: d.UpstreamModelID,
			GatewayID:       r.GatewayID,
			DisplayName:     r.DisplayName,
			Metadata:        r.Metadata,
		})
	}

	if _, err := h.store.ReplaceModels(ctx, upstreamID, models); err != nil {
		h.logger.Error("provider registry sync failed", "provider", name, "err", err)
		h.markProviderReachable(name, false, err.Error())
		return 0, "models could not be stored: " + err.Error()
	}
	h.markProviderReachable(name, true, "")
	return len(models), ""
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
