package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrKeyNotFound is returned when no active virtual key matches a hash.
var ErrKeyNotFound = errors.New("virtual key not found")

// KeyFilter constrains a key to (or away from) a set of providers or models.
// Mode is "none" (no constraint), "include" (only Values) or "exclude"
// (everything but Values).
type KeyFilter struct {
	Mode   string   `json:"mode"`
	Values []string `json:"values"`
}

// normalize returns the filter with a concrete mode and non-nil values.
func (f KeyFilter) normalize() KeyFilter {
	switch f.Mode {
	case "include", "exclude":
	default:
		f.Mode = "none"
	}
	if f.Values == nil {
		f.Values = []string{}
	}
	if f.Mode == "none" {
		f.Values = []string{}
	}
	return f
}

// CreateVirtualKey stores a new key (hash only) and returns its id. The key's
// provider and model filters come from the referenced profile, resolved on
// every lookup, so edits to a profile apply immediately to all keys using it.
func (s *Store) CreateVirtualKey(ctx context.Context, name, keyHash string, profileID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO virtual_keys (name, key_hash, profile_id) VALUES (?, ?, ?)`,
		name, keyHash, profileID)
	if err != nil {
		return 0, fmt.Errorf("create virtual key %q: %w", name, err)
	}
	return res.LastInsertId()
}

// VirtualKey is a stored key row with its profile's filters resolved.
type VirtualKey struct {
	ID             int64
	Name           string
	ProfileID      int64
	ProfileName    string
	ProviderFilter KeyFilter
	ModelFilter    KeyFilter
	Revoked        bool
	Paused         bool
}

// scanKey reads a key row from a query selecting id, name, profile_id,
// profile name, provider_filter, model_filter — the filters joined from the
// key's profile.
func scanKey(row interface{ Scan(...any) error }) (VirtualKey, error) {
	var vk VirtualKey
	var provider, model string
	if err := row.Scan(&vk.ID, &vk.Name, &vk.ProfileID, &vk.ProfileName, &provider, &model); err != nil {
		return vk, err
	}
	_ = json.Unmarshal([]byte(provider), &vk.ProviderFilter)
	_ = json.Unmarshal([]byte(model), &vk.ModelFilter)
	vk.ProviderFilter = vk.ProviderFilter.normalize()
	vk.ModelFilter = vk.ModelFilter.normalize()
	return vk, nil
}

// KeyByHash resolves an active key by plaintext hash. Revoked and paused
// keys are both treated as inactive.
func (s *Store) KeyByHash(ctx context.Context, keyHash string) (*VirtualKey, error) {
	vk, err := scanKey(s.db.QueryRowContext(ctx, `
		SELECT vk.id, vk.name, vk.profile_id, p.name, p.provider_filter, p.model_filter
		FROM virtual_keys vk JOIN profiles p ON p.id = vk.profile_id
		WHERE vk.key_hash = ? AND vk.revoked_at IS NULL AND vk.paused_at IS NULL`, keyHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup key: %w", err)
	}
	return &vk, nil
}

// ListVirtualKeys returns all keys, including revoked and paused ones, with
// their profiles resolved.
func (s *Store) ListVirtualKeys(ctx context.Context) ([]VirtualKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT vk.id, vk.name, vk.profile_id, p.name, p.provider_filter, p.model_filter,
		       vk.revoked_at, vk.paused_at
		FROM virtual_keys vk JOIN profiles p ON p.id = vk.profile_id
		ORDER BY vk.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VirtualKey
	for rows.Next() {
		var vk VirtualKey
		var provider, model string
		var revoked, paused sql.NullString
		if err := rows.Scan(&vk.ID, &vk.Name, &vk.ProfileID, &vk.ProfileName, &provider, &model, &revoked, &paused); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(provider), &vk.ProviderFilter)
		_ = json.Unmarshal([]byte(model), &vk.ModelFilter)
		vk.ProviderFilter = vk.ProviderFilter.normalize()
		vk.ModelFilter = vk.ModelFilter.normalize()
		vk.Revoked = revoked.Valid
		vk.Paused = paused.Valid
		out = append(out, vk)
	}
	return out, rows.Err()
}

// UpdateVirtualKey edits a key's name and/or profile in place. The key
// material (its hash) is unchanged, so existing clients keep working. Returns
// ErrKeyNotFound when currentName does not exist; a rename onto an existing
// name fails on the UNIQUE constraint.
func (s *Store) UpdateVirtualKey(ctx context.Context, currentName, newName string, profileID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE virtual_keys SET name = ?, profile_id = ?
		WHERE name = ?`,
		newName, profileID, currentName)
	if err != nil {
		return fmt.Errorf("update virtual key %q: %w", currentName, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update virtual key %q: %w", currentName, err)
	}
	if n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// RevokeVirtualKey marks a key revoked (no-op if already revoked).
func (s *Store) RevokeVirtualKey(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE virtual_keys SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE name = ? AND revoked_at IS NULL`, name)
	if err != nil {
		return fmt.Errorf("revoke virtual key %q: %w", name, err)
	}
	return nil
}

// PauseVirtualKey temporarily disables a key without revoking it. Pausing a
// revoked key is a no-op (it is already inactive).
func (s *Store) PauseVirtualKey(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE virtual_keys SET paused_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE name = ? AND revoked_at IS NULL AND paused_at IS NULL`, name)
	if err != nil {
		return fmt.Errorf("pause virtual key %q: %w", name, err)
	}
	return nil
}

// ResumeVirtualKey re-enables a paused key.
func (s *Store) ResumeVirtualKey(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE virtual_keys SET paused_at = NULL
		WHERE name = ? AND paused_at IS NOT NULL`, name)
	if err != nil {
		return fmt.Errorf("resume virtual key %q: %w", name, err)
	}
	return nil
}

// DeleteVirtualKey permanently removes a key. Its usage and conversation
// history is kept, with the now-dangling key reference cleared.
func (s *Store) DeleteVirtualKey(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete virtual key %q: begin: %w", name, err)
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM virtual_keys WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("delete virtual key %q: lookup: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_events SET key_id = NULL WHERE key_id = ?`, id); err != nil {
		return fmt.Errorf("delete virtual key %q: detach usage: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET key_id = NULL WHERE key_id = ?`, id); err != nil {
		return fmt.Errorf("delete virtual key %q: detach conversations: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM virtual_keys WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete virtual key %q: %w", name, err)
	}
	return tx.Commit()
}

// ModelRow is one registry entry as served by /v1/models.
type ModelRow struct {
	ID                int64
	UpstreamName      string
	UpstreamModelID   string
	GatewayID         string
	DisplayName       string
	Metadata          string // merged metadata JSON (id still the upstream one)
	Disabled          bool   // the model itself
	UpstreamDisabled  bool   // its provider is disabled
	UpstreamReachable bool   // its provider was reachable on the last sync
	Alias             string // custom gateway-ID alias, empty when unset
}

// ListModels returns every registered model across all upstreams, including
// provider status, so callers can decide visibility.
func (s *Store) ListModels(ctx context.Context) ([]ModelRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, u.name, m.upstream_model_id, m.gateway_id, m.display_name, m.metadata,
		       m.disabled, u.disabled, u.reachable, COALESCE(ma.alias, '')
		FROM models m JOIN upstreams u ON u.id = m.upstream_id
		LEFT JOIN model_aliases ma
		       ON ma.upstream_id = m.upstream_id AND ma.upstream_model_id = m.upstream_model_id
		ORDER BY m.gateway_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelRow
	for rows.Next() {
		var m ModelRow
		if err := rows.Scan(&m.ID, &m.UpstreamName, &m.UpstreamModelID, &m.GatewayID, &m.DisplayName, &m.Metadata,
			&m.Disabled, &m.UpstreamDisabled, &m.UpstreamReachable, &m.Alias); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpstreamRow is one upstream with registry counts and sync status, for the
// admin UI.
type UpstreamRow struct {
	ID             int64
	Name           string
	BaseURL        string
	Position       int
	ModelCount     int
	RefreshSeconds int
	Disabled       bool
	Reachable      bool
	LastError      string
	LastSyncedAt   string
}

// SyncUpstream carries the credentials needed to re-pull an upstream's model
// catalog (admin-triggered refresh).
type SyncUpstream struct {
	ID      int64
	Name    string
	BaseURL string
	APIKey  string
}

// ListUpstreamsForSync returns every upstream with its base URL and API key,
// in config order, for a full registry re-discovery.
func (s *Store) ListUpstreamsForSync(ctx context.Context) ([]SyncUpstream, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, base_url, api_key FROM upstreams ORDER BY position, id`)
	if err != nil {
		return nil, fmt.Errorf("list upstreams for sync: %w", err)
	}
	defer rows.Close()
	var out []SyncUpstream
	for rows.Next() {
		var u SyncUpstream
		if err := rows.Scan(&u.ID, &u.Name, &u.BaseURL, &u.APIKey); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListUpstreams returns all configured upstreams with their registry sizes.
func (s *Store) ListUpstreams(ctx context.Context) ([]UpstreamRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.name, u.base_url, u.position, u.refresh_seconds,
		       u.disabled, u.reachable, u.last_error, COALESCE(u.last_synced_at, ''),
		       COUNT(m.id)
		FROM upstreams u LEFT JOIN models m ON m.upstream_id = u.id
		GROUP BY u.id ORDER BY u.position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamRow
	for rows.Next() {
		var u UpstreamRow
		if err := rows.Scan(&u.ID, &u.Name, &u.BaseURL, &u.Position, &u.RefreshSeconds,
			&u.Disabled, &u.Reachable, &u.LastError, &u.LastSyncedAt, &u.ModelCount); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUpstreamDisabled enables or disables a provider. A disabled provider
// behaves like a disabled model for all of its models: hidden from /v1/models
// and refusing routing. Errors with ErrUpstreamNotFound for an unknown name.
func (s *Store) SetUpstreamDisabled(ctx context.Context, name string, disabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE upstreams SET disabled = ? WHERE name = ?`, disabled, name)
	if err != nil {
		return fmt.Errorf("set upstream %q disabled=%v: %w", name, disabled, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrUpstreamNotFound
	}
	return nil
}

// SetUpstreamReachable records the outcome of a discovery sync. On success it
// stamps last_synced_at and clears last_error. On failure it keeps the
// last-known models but marks the provider unreachable and stores the error.
// A provider not yet in the store is silently ignored.
func (s *Store) SetUpstreamReachable(ctx context.Context, name string, reachable bool, errMsg string) error {
	var err error
	if reachable {
		_, err = s.db.ExecContext(ctx, `
			UPDATE upstreams SET reachable = 1, last_error = '',
			       last_synced_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE name = ?`, name)
	} else {
		_, err = s.db.ExecContext(ctx, `UPDATE upstreams SET reachable = 0, last_error = ? WHERE name = ?`, errMsg, name)
	}
	if err != nil {
		return fmt.Errorf("set upstream %q reachable=%v: %w", name, reachable, err)
	}
	return nil
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// KeyFilter and []string always marshal; unreachable.
		return "{}"
	}
	return string(b)
}
