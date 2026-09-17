// Package discovery keeps the model registry in sync with each upstream's
// GET /v1/models. Metadata is stored verbatim — the registry is the source
// of truth the gateway re-emits in its own /v1/models.
//
// Failure policy (per design): if an upstream is down or does not implement
// model listing, the previous registry state is kept and the syncer logs and
// retries on the next tick. No failover, no crash.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/charmbracelet/log"

	"github.com/trfdeer/toll/internal/config"
	"github.com/trfdeer/toll/internal/registry"
	"github.com/trfdeer/toll/internal/store"
)

// fetchTimeout bounds a single upstream /models call.
const fetchTimeout = 30 * time.Second

// Client fetches model catalogs from OpenAI-compatible upstreams.
type Client struct {
	hc *http.Client
}

func NewClient() *Client {
	return &Client{hc: &http.Client{Timeout: fetchTimeout}}
}

// Models fetches and parses the upstream's model list. Each entry's JSON is
// returned byte-identical to what the upstream sent.
func (c *Client) Models(ctx context.Context, base *url.URL, apiKey string) ([]store.DiscoveredModel, error) {
	u := *base
	u.Path = joinModelPath(base.Path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("GET %s: status %d: %s", u.Redacted(), resp.StatusCode, body)
	}

	var catalog struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}

	models := make([]store.DiscoveredModel, 0, len(catalog.Data))
	for _, raw := range catalog.Data {
		var entry struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil || entry.ID == "" {
			// Not a model entry (or malformed) — skip it, keep the rest.
			continue
		}
		models = append(models, store.DiscoveredModel{
			UpstreamModelID: entry.ID,
			Metadata:        raw,
		})
	}
	return models, nil
}

// Syncer reconciles the registry with every configured upstream, at startup
// and then on each upstream's refresh interval. Discovered models pass
// through the alias engine (rules, entries, overlays) before landing in the
// registry.
type Syncer struct {
	store   *store.Store
	cfg     *config.Config
	client  *Client
	logger  *log.Logger
	engines map[string]*registry.Engine // by upstream name
	seeded  map[string]bool             // config aliases seeded per upstream
}

func NewSyncer(st *store.Store, cfg *config.Config, logger *log.Logger) *Syncer {
	engines := make(map[string]*registry.Engine, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		engines[u.Name] = registry.NewEngine(u)
	}
	return &Syncer{
		store: st, cfg: cfg, client: NewClient(), logger: logger,
		engines: engines, seeded: make(map[string]bool, len(cfg.Upstreams)),
	}
}

// Run blocks until ctx is cancelled, syncing each upstream on its own
// schedule.
func (s *Syncer) Run(ctx context.Context) {
	for _, u := range s.cfg.Upstreams {
		u := u
		go s.loop(ctx, u)
	}
	<-ctx.Done()
}

func (s *Syncer) loop(ctx context.Context, u *config.Upstream) {
	s.sync(ctx, u) // initial sync at startup
	if u.DisableRefresh {
		return
	}
	t := time.NewTicker(u.Refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sync(ctx, u)
		}
	}
}

// sync fetches the catalog, resolves each model through the alias engine
// and reconciles the registry for one upstream.
func (s *Syncer) sync(ctx context.Context, u *config.Upstream) {
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	discovered, err := s.client.Models(fetchCtx, u.URL, u.APIKey)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("model discovery failed; keeping previous registry state",
				"upstream", u.Name, "err", err)
		}
		s.markUnreachable(u.Name, err)
		return
	}

	engine := s.engines[u.Name]
	models := make([]store.DiscoveredModel, 0, len(discovered))
	for _, d := range discovered {
		r, err := engine.Resolve(d.UpstreamModelID, d.Metadata)
		if err != nil {
			s.logger.Warn("skipping model with unresolvable metadata", "upstream", u.Name, "err", err)
			continue
		}
		models = append(models, store.DiscoveredModel{
			UpstreamModelID: d.UpstreamModelID,
			GatewayID:       r.GatewayID,
			DisplayName:     r.DisplayName,
			Metadata:        r.Metadata,
			Disabled:        r.Disabled,
		})
	}

	if err := s.replace(ctx, u, models); err != nil {
		s.logger.Error("model registry sync failed", "upstream", u.Name, "err", err)
		return
	}
	s.logger.Debug("model discovery synced", "upstream", u.Name, "models", len(models))
}

func (s *Syncer) replace(ctx context.Context, u *config.Upstream, models []store.DiscoveredModel) error {
	// Resolve the upstream row (registers config-defined upstreams in the DB).
	id, err := s.store.UpsertUpstream(ctx, u.Name, u.URL.String(), u.APIKey, int(u.Refresh.Seconds()), u.Position)
	if err != nil {
		return err
	}
	// Config aliases seed the DB once, at startup. Afterwards the DB is the
	// source of truth (admin edits win until the next restart).
	if !s.seeded[u.Name] {
		if err := s.store.SeedModelAliases(ctx, id, configAliases(u)); err != nil {
			return err
		}
		s.seeded[u.Name] = true
	}
	skipped, err := s.store.ReplaceModels(ctx, id, models)
	if err != nil {
		return err
	}
	if skipped > 0 {
		s.logger.Warn("alias collisions: gateway IDs already owned by an earlier upstream",
			"upstream", u.Name, "skipped", skipped)
	}
	return nil
}

// configAliases maps an upstream's config entries (upstream model ID → alias)
// for seeding. An empty alias clears any stored one.
func configAliases(u *config.Upstream) map[string]string {
	if len(u.Models) == 0 {
		return nil
	}
	out := make(map[string]string, len(u.Models))
	for _, m := range u.Models {
		out[m.ID] = m.Alias
	}
	return out
}

// markUnreachable records a failed discovery on a detached context, so a
// cancelled request context doesn't drop the status update. The provider's
// last-known models stay in the registry but become unroutable until the next
// successful sync.
func (s *Syncer) markUnreachable(name string, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.SetUpstreamReachable(ctx, name, false, cause.Error()); err != nil {
		s.logger.Error("mark upstream unreachable", "upstream", name, "err", err)
	}
}

// joinModelPath appends /models to the upstream base path (…/v1).
func joinModelPath(base string) string {
	if base == "" || base == "/" {
		return "/models"
	}
	return base + "/models"
}
