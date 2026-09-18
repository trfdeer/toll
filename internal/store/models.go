package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// DiscoveredModel is one model in the registry, as produced by the alias
// engine: the upstream's model ID, its resolved gateway-facing form, the
// merged metadata JSON and the config-pinned disabled state (nil when the
// config does not pin it).
type DiscoveredModel struct {
	UpstreamModelID string
	GatewayID       string
	DisplayName     string
	Metadata        []byte
	Disabled        *bool
}

// UpsertUpstream inserts or updates the upstream row (including its config
// position, used for collision resolution) and returns its id.
//
// The resolved API key is stored alongside the config: the DB lives in the
// same trust domain as the process environment, and the admin UI needs to
// show upstream status. Keys are rotated via the environment.
func (s *Store) UpsertUpstream(ctx context.Context, name, baseURL, apiKey string, refreshSeconds, position int) (int64, error) {
	// RETURNING id is required: LastInsertId is not reliable on the
	// ON CONFLICT DO UPDATE path.
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO upstreams (name, base_url, api_key, refresh_seconds, position, reachable, last_error, last_synced_at)
		VALUES (?, ?, ?, ?, ?, 1, '', strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(name) DO UPDATE SET
			base_url = excluded.base_url,
			api_key = excluded.api_key,
			refresh_seconds = excluded.refresh_seconds,
			position = excluded.position,
			reachable = 1,
			last_error = '',
			last_synced_at = excluded.last_synced_at
		RETURNING id`,
		name, baseURL, apiKey, refreshSeconds, position).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert upstream %q: %w", name, err)
	}
	return id, nil
}

// ReplaceModels atomically syncs the registry for one upstream to exactly
// the given set: inserts new models, updates changed metadata, removes
// models the upstream no longer reports.
//
// Cross-upstream alias collisions: when two upstreams resolve to the same
// gateway ID, the upstream earlier in config order (lower position) wins —
// colliding inserts from later upstreams are skipped. Returns the number of
// skipped collisions.
func (s *Store) ReplaceModels(ctx context.Context, upstreamID int64, models []DiscoveredModel) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sync models: begin: %w", err)
	}
	defer tx.Rollback()

	var position int
	if err := tx.QueryRowContext(ctx, `SELECT position FROM upstreams WHERE id = ?`, upstreamID).Scan(&position); err != nil {
		return 0, fmt.Errorf("sync models: upstream %d: %w", upstreamID, err)
	}

	// Custom aliases (set from the admin UI or seeded from config) override
	// the computed gateway ID. base_gateway_id keeps the computed one so an
	// alias can be cleared without another discovery pass.
	aliases := map[string]string{}
	{
		rows, err := tx.QueryContext(ctx, `SELECT upstream_model_id, alias FROM model_aliases WHERE upstream_id = ?`, upstreamID)
		if err != nil {
			return 0, fmt.Errorf("sync models: load aliases: %w", err)
		}
		for rows.Next() {
			var id, alias string
			if err := rows.Scan(&id, &alias); err != nil {
				rows.Close()
				return 0, fmt.Errorf("sync models: load aliases: %w", err)
			}
			aliases[id] = alias
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sync models: load aliases: %w", err)
		}
		rows.Close()
	}

	skipped := 0
	for _, m := range models {
		gatewayID := m.GatewayID
		if alias := aliases[m.UpstreamModelID]; alias != "" {
			gatewayID = alias
		}

		// Collision check: does a different upstream already own this
		// gateway ID, and does it outrank us?
		var otherID int64
		var otherPos int
		err := tx.QueryRowContext(ctx, `
			SELECT m.upstream_id, u.position
			FROM models m JOIN upstreams u ON u.id = m.upstream_id
			WHERE m.gateway_id = ? AND m.upstream_id != ?`,
			gatewayID, upstreamID).Scan(&otherID, &otherPos)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// no conflict
		case err != nil:
			return 0, fmt.Errorf("sync models: collision check %q: %w", gatewayID, err)
		case otherPos <= position:
			skipped++
			continue // first upstream in config order wins
		default:
			// We outrank the existing owner: take the gateway ID over.
			if _, err := tx.ExecContext(ctx, `DELETE FROM models WHERE upstream_id = ? AND gateway_id = ?`, otherID, gatewayID); err != nil {
				return 0, fmt.Errorf("sync models: displace collision %q: %w", gatewayID, err)
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO models (upstream_id, gateway_id, base_gateway_id, upstream_model_id, display_name, metadata, disabled, discovered_at)
			VALUES (?, ?, ?, ?, ?, ?, COALESCE(?, 0), strftime('%Y-%m-%dT%H:%M:%fZ','now'))
			ON CONFLICT(upstream_id, upstream_model_id) DO UPDATE SET
				gateway_id = excluded.gateway_id,
				base_gateway_id = excluded.base_gateway_id,
				display_name = excluded.display_name,
				metadata = excluded.metadata,
				disabled = CASE WHEN ? IS NULL THEN models.disabled ELSE excluded.disabled END,
				discovered_at = excluded.discovered_at`,
			upstreamID, gatewayID, m.GatewayID, m.UpstreamModelID, m.DisplayName, string(m.Metadata), m.Disabled, m.Disabled); err != nil {
			return 0, fmt.Errorf("sync models: upsert %q: %w", m.UpstreamModelID, err)
		}
	}

	// Delete models no longer reported by the upstream. Compare against the
	// full reported set in memory, then delete the stale IDs in chunks. A
	// chunked `NOT IN (chunk)` would be wrong: each chunk would delete the
	// rows belonging to the other chunks (regression: catalogs >400 models
	// were wiped on every sync).
	reported := make(map[string]struct{}, len(models))
	for _, m := range models {
		reported[m.UpstreamModelID] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `SELECT upstream_model_id FROM models WHERE upstream_id = ?`, upstreamID)
	if err != nil {
		return 0, fmt.Errorf("sync models: select stale: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sync models: select stale: %w", err)
		}
		if _, ok := reported[id]; !ok {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("sync models: select stale: %w", err)
	}
	rows.Close()

	const chunk = 400
	for start := 0; start < len(stale); start += chunk {
		end := start + chunk
		if end > len(stale) {
			end = len(stale)
		}
		batch := stale[start:end]
		qmarks := "(" + strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",") + ")"
		args := append([]any{upstreamID}, toAny(batch)...)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM models WHERE upstream_id = ? AND upstream_model_id IN `+qmarks, args...); err != nil {
			return 0, fmt.Errorf("sync models: delete stale: %w", err)
		}
	}

	return skipped, tx.Commit()
}

// ExportModel is one registry entry as needed to regenerate a config file:
// which upstream it belongs to, the pinned upstream model ID and the
// resolved gateway-facing ID/name plus the merged metadata JSON.
type ExportModel struct {
	UpstreamID      int64
	UpstreamModelID string
	GatewayID       string
	DisplayName     string
	Metadata        string
	Disabled        bool
	Alias           string // custom alias, empty when the model uses its computed ID
}

// ListModelsForExport returns every registry entry across all upstreams,
// ordered by upstream and gateway ID, for config export.
func (s *Store) ListModelsForExport(ctx context.Context) ([]ExportModel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.upstream_id, m.upstream_model_id, m.gateway_id, m.display_name, m.metadata, m.disabled,
		       COALESCE(ma.alias, '')
		FROM models m
		LEFT JOIN model_aliases ma
		       ON ma.upstream_id = m.upstream_id AND ma.upstream_model_id = m.upstream_model_id
		ORDER BY m.upstream_id, m.gateway_id`)
	if err != nil {
		return nil, fmt.Errorf("list models for export: %w", err)
	}
	defer rows.Close()
	var out []ExportModel
	for rows.Next() {
		var m ExportModel
		if err := rows.Scan(&m.UpstreamID, &m.UpstreamModelID, &m.GatewayID, &m.DisplayName, &m.Metadata, &m.Disabled, &m.Alias); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ErrUpstreamNotFound is returned when no upstream matches a name.
var ErrUpstreamNotFound = errors.New("upstream not found")

// DeleteUpstream removes an upstream and, via ON DELETE CASCADE, its models.
func (s *Store) DeleteUpstream(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM upstreams WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete upstream %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete upstream %q: %w", name, err)
	}
	if n == 0 {
		return ErrUpstreamNotFound
	}
	return nil
}

// NextUpstreamPosition returns one past the highest configured position, so a
// newly added upstream sorts after existing ones (collision resolution keeps
// first-in-config-order winning).
func (s *Store) NextUpstreamPosition(ctx context.Context) (int, error) {
	var next int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position), -1) + 1 FROM upstreams`).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("next upstream position: %w", err)
	}
	return next, nil
}

// DeleteModel removes one registry entry by id.
func (s *Store) DeleteModel(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete model %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete model %d: %w", id, err)
	}
	if n == 0 {
		return ErrModelNotFound
	}
	return nil
}

// SetModelDisabled enables or disables one registry entry. A disabled model
// stays registered (and is exported to config) but is hidden from /v1/models
// and cannot be routed.
func (s *Store) SetModelDisabled(ctx context.Context, id int64, disabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE models SET disabled = ? WHERE id = ?`, disabled, id)
	if err != nil {
		return fmt.Errorf("set model %d disabled=%v: %w", id, disabled, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set model %d disabled=%v: %w", id, disabled, err)
	}
	if n == 0 {
		return ErrModelNotFound
	}
	return nil
}

// ModelCount returns the number of registered models for an upstream.
func (s *Store) ModelCount(ctx context.Context, upstreamID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM models WHERE upstream_id = ?`, upstreamID).Scan(&n)
	return n, err
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
