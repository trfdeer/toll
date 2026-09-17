package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrAliasConflict is returned when a custom alias is already the gateway ID
// of another model.
var ErrAliasConflict = errors.New("alias already in use")

// SeedModelAliases writes config-defined aliases into the DB at startup. A
// non-empty alias is upserted; an empty one clears any stored alias. Config is
// only the seed — once running, the DB is authoritative for alias resolution.
func (s *Store) SeedModelAliases(ctx context.Context, upstreamID int64, aliases map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("seed aliases: begin: %w", err)
	}
	defer tx.Rollback()
	for modelID, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM model_aliases WHERE upstream_id = ? AND upstream_model_id = ?`,
				upstreamID, modelID); err != nil {
				return fmt.Errorf("seed aliases: clear %q: %w", modelID, err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_aliases (upstream_id, upstream_model_id, alias) VALUES (?, ?, ?)
			ON CONFLICT(upstream_id, upstream_model_id) DO UPDATE SET alias = excluded.alias`,
			upstreamID, modelID, alias); err != nil {
			return fmt.Errorf("seed aliases: set %q: %w", modelID, err)
		}
	}
	return tx.Commit()
}

// SetModelAlias sets or clears one model's custom gateway ID. An empty alias
// restores the model's computed ID. Returns ErrModelNotFound when the model is
// gone, or ErrAliasConflict when the alias is another model's gateway ID.
func (s *Store) SetModelAlias(ctx context.Context, modelID int64, alias string) error {
	alias = strings.TrimSpace(alias)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set alias: begin: %w", err)
	}
	defer tx.Rollback()

	var upstreamID int64
	var upstreamModelID, base string
	err = tx.QueryRowContext(ctx,
		`SELECT upstream_id, upstream_model_id, base_gateway_id FROM models WHERE id = ?`, modelID).
		Scan(&upstreamID, &upstreamModelID, &base)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrModelNotFound
	}
	if err != nil {
		return fmt.Errorf("set alias: lookup model %d: %w", modelID, err)
	}

	// Alias equal to the computed ID is the same as no alias.
	if alias == base {
		alias = ""
	}

	if alias == "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM model_aliases WHERE upstream_id = ? AND upstream_model_id = ?`,
			upstreamID, upstreamModelID); err != nil {
			return fmt.Errorf("set alias: clear: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE models SET gateway_id = ? WHERE id = ?`, base, modelID); err != nil {
			return fmt.Errorf("set alias: restore: %w", err)
		}
		return tx.Commit()
	}

	var other int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM models WHERE gateway_id = ? AND id != ?`, alias, modelID).Scan(&other)
	switch {
	case err == nil:
		return ErrAliasConflict
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("set alias: conflict check: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_aliases (upstream_id, upstream_model_id, alias) VALUES (?, ?, ?)
		ON CONFLICT(upstream_id, upstream_model_id) DO UPDATE SET alias = excluded.alias`,
		upstreamID, upstreamModelID, alias); err != nil {
		return fmt.Errorf("set alias: store: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE models SET gateway_id = ? WHERE id = ?`, alias, modelID); err != nil {
		return fmt.Errorf("set alias: apply: %w", err)
	}
	return tx.Commit()
}
