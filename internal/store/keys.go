package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// VirtualKey is a stored key row with its profile's rules resolved. A key
// allows a model when AllowAll is set or any of Rules allows it.
type VirtualKey struct {
	ID          int64
	Name        string
	ProfileID   int64
	ProfileName string
	// AllowAll is set when the profile (or any ancestor) is the All profile:
	// every model is permitted.
	AllowAll bool
	// Rules is the resolved union of leaf clauses the profile permits.
	Rules   []KeyRule
	Revoked bool
	Paused  bool
}

// KeyByHash resolves an active key by plaintext hash. Revoked and paused
// keys are both treated as inactive.
func (s *Store) KeyByHash(ctx context.Context, keyHash string) (*VirtualKey, error) {
	var vk VirtualKey
	err := s.db.QueryRowContext(ctx, `
		SELECT vk.id, vk.name, vk.profile_id, p.name
		FROM virtual_keys vk JOIN profiles p ON p.id = vk.profile_id
		WHERE vk.key_hash = ? AND vk.revoked_at IS NULL AND vk.paused_at IS NULL`, keyHash).
		Scan(&vk.ID, &vk.Name, &vk.ProfileID, &vk.ProfileName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup key: %w", err)
	}
	graph, err := s.loadProfileGraph(ctx)
	if err != nil {
		return nil, err
	}
	vk.AllowAll, vk.Rules = resolveProfileRules(graph, vk.ProfileID)
	return &vk, nil
}

// virtualKeyFilterCols and virtualKeySortCols map the admin column ids to SQL
// expressions for the keys table. Status is derived from the timestamps.
var (
	virtualKeyStatusExpr = "(CASE WHEN vk.revoked_at IS NOT NULL THEN 'revoked' WHEN vk.paused_at IS NOT NULL THEN 'paused' ELSE 'active' END)"
	virtualKeyFilterCols = map[string]string{
		"name":    "vk.name",
		"profile": "p.name",
		"status":  virtualKeyStatusExpr,
	}
	virtualKeySortCols = map[string]string{
		"id":      "vk.id",
		"name":    "vk.name",
		"profile": "p.name",
		"status":  virtualKeyStatusExpr,
	}
)

// ListVirtualKeysPaged returns a page of keys, including revoked and paused
// ones, with their profiles resolved, plus the total before windowing.
func (s *Store) ListVirtualKeysPaged(ctx context.Context, p ListParams) ([]VirtualKey, int, error) {
	// Resolve the profile graph first: the outer query below keeps the pool's
	// single connection open for its duration.
	graph, err := s.loadProfileGraph(ctx)
	if err != nil {
		return nil, 0, err
	}
	fconds, fargs, err := buildFilters(p.Filter, virtualKeyFilterCols)
	if err != nil {
		return nil, 0, err
	}
	where := ""
	if len(fconds) > 0 {
		where = "WHERE " + strings.Join(fconds, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM virtual_keys vk JOIN profiles p ON p.id = vk.profile_id
		`+where, fargs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order, err := buildOrder(p, virtualKeySortCols, "id", "asc", "vk.id")
	if err != nil {
		return nil, 0, err
	}
	query := `
		SELECT vk.id, vk.name, vk.profile_id, p.name,
		       vk.revoked_at, vk.paused_at
		FROM virtual_keys vk JOIN profiles p ON p.id = vk.profile_id
		` + where + order
	if limit := p.pageLimit(50, 1000); limit > 0 {
		query += " LIMIT ? OFFSET ?"
		fargs = append(fargs, limit, p.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, fargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []VirtualKey
	for rows.Next() {
		var vk VirtualKey
		var revoked, paused sql.NullString
		if err := rows.Scan(&vk.ID, &vk.Name, &vk.ProfileID, &vk.ProfileName, &revoked, &paused); err != nil {
			return nil, 0, err
		}
		vk.AllowAll, vk.Rules = resolveProfileRules(graph, vk.ProfileID)
		vk.Revoked = revoked.Valid
		vk.Paused = paused.Valid
		out = append(out, vk)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListVirtualKeys returns every key (no paging), for callers that need the
// full set.
func (s *Store) ListVirtualKeys(ctx context.Context) ([]VirtualKey, error) {
	keys, _, err := s.ListVirtualKeysPaged(ctx, ListParams{Limit: -1})
	return keys, err
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

// modelStatusExpr derives a model's effective status in SQL, matching the
// admin UI's modelStatus(): the model, its provider, or a failed sync.
const modelStatusExpr = "(CASE WHEN m.disabled = 1 THEN 'disabled' " +
	"WHEN m.upstream_disabled = 1 THEN 'provider disabled' " +
	"WHEN m.upstream_reachable = 0 THEN 'unreachable' ELSE 'active' END)"

// modelFilterCols and modelSortCols map the admin column ids to columns of the
// model_search view, which exposes the metadata-derived limits and price. Cost
// sorts/filters on the numeric input price (the cell still renders the full
// pricing summary).
var (
	modelFilterCols = map[string]string{
		"gatewayId":   "m.gateway_id",
		"upstream":    "m.upstream_name",
		"displayName": "m.display_name",
		"alias":       "m.alias",
		"status":      modelStatusExpr,
		"inputLimit":  "m.input_limit",
		"outputLimit": "m.output_limit",
		"cost":        "m.input_price",
	}
	modelSortCols = map[string]string{
		"id":          "m.id",
		"gatewayId":   "m.gateway_id",
		"upstream":    "m.upstream_name",
		"displayName": "m.display_name",
		"alias":       "m.alias",
		"status":      modelStatusExpr,
		"inputLimit":  "m.input_limit",
		"outputLimit": "m.output_limit",
		"cost":        "m.input_price",
	}
)

// ListModelsPaged returns a page of registered models across all upstreams,
// including provider status, plus the total before windowing.
func (s *Store) ListModelsPaged(ctx context.Context, p ListParams) ([]ModelRow, int, error) {
	fconds, fargs, err := buildFilters(p.Filter, modelFilterCols)
	if err != nil {
		return nil, 0, err
	}
	where := ""
	if len(fconds) > 0 {
		where = "WHERE " + strings.Join(fconds, " AND ")
	}

	const from = ` FROM model_search m`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from+` `+where, fargs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order, err := buildOrder(p, modelSortCols, "gatewayId", "asc", "m.id")
	if err != nil {
		return nil, 0, err
	}
	query := `
		SELECT m.id, m.upstream_name, m.upstream_model_id, m.gateway_id, m.display_name, m.metadata,
		       m.disabled, m.upstream_disabled, m.upstream_reachable, m.alias` + from + ` ` + where + order
	if limit := p.pageLimit(50, 1000); limit > 0 {
		query += " LIMIT ? OFFSET ?"
		fargs = append(fargs, limit, p.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, fargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []ModelRow
	for rows.Next() {
		var m ModelRow
		if err := rows.Scan(&m.ID, &m.UpstreamName, &m.UpstreamModelID, &m.GatewayID, &m.DisplayName, &m.Metadata,
			&m.Disabled, &m.UpstreamDisabled, &m.UpstreamReachable, &m.Alias); err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListModels returns every registered model (no paging), for callers that need
// the full set.
func (s *Store) ListModels(ctx context.Context) ([]ModelRow, error) {
	rows, _, err := s.ListModelsPaged(ctx, ListParams{Limit: -1})
	return rows, err
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

// upstreamStatusExpr derives a provider's status in SQL, matching status().
const upstreamStatusExpr = "(CASE WHEN u.disabled = 1 THEN 'disabled' " +
	"WHEN u.reachable = 0 THEN 'unreachable' ELSE 'active' END)"

// upstreamFilterCols and upstreamSortCols map the admin column ids to SQL
// expressions. modelCount is an aggregate, valid in ORDER BY under GROUP BY.
var (
	upstreamFilterCols = map[string]string{
		"name":    "u.name",
		"baseURL": "u.base_url",
		"status":  upstreamStatusExpr,
	}
	upstreamSortCols = map[string]string{
		"id":         "u.id",
		"position":   "u.position",
		"name":       "u.name",
		"baseURL":    "u.base_url",
		"modelCount": "COUNT(m.id)",
		"status":     upstreamStatusExpr,
	}
)

// ListUpstreamsPaged returns a page of configured upstreams with their registry
// sizes, plus the total before windowing.
func (s *Store) ListUpstreamsPaged(ctx context.Context, p ListParams) ([]UpstreamRow, int, error) {
	fconds, fargs, err := buildFilters(p.Filter, upstreamFilterCols)
	if err != nil {
		return nil, 0, err
	}
	where := ""
	if len(fconds) > 0 {
		where = "WHERE " + strings.Join(fconds, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM upstreams u `+where, fargs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order, err := buildOrder(p, upstreamSortCols, "position", "asc", "u.id")
	if err != nil {
		return nil, 0, err
	}
	query := `
		SELECT u.id, u.name, u.base_url, u.position, u.refresh_seconds,
		       u.disabled, u.reachable, u.last_error, COALESCE(u.last_synced_at, ''),
		       COUNT(m.id)
		FROM upstreams u LEFT JOIN models m ON m.upstream_id = u.id
		` + where + `
		GROUP BY u.id` + order
	if limit := p.pageLimit(50, 1000); limit > 0 {
		query += " LIMIT ? OFFSET ?"
		fargs = append(fargs, limit, p.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, fargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []UpstreamRow
	for rows.Next() {
		var u UpstreamRow
		if err := rows.Scan(&u.ID, &u.Name, &u.BaseURL, &u.Position, &u.RefreshSeconds,
			&u.Disabled, &u.Reachable, &u.LastError, &u.LastSyncedAt, &u.ModelCount); err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListUpstreams returns every configured upstream (no paging), for callers
// that need the full set.
func (s *Store) ListUpstreams(ctx context.Context) ([]UpstreamRow, error) {
	ups, _, err := s.ListUpstreamsPaged(ctx, ListParams{Limit: -1})
	return ups, err
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
