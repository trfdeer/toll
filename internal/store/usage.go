package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrModelNotFound is returned when a gateway model ID is not in the registry.
var ErrModelNotFound = errors.New("model not found")

// Route is the resolved destination for a gateway model ID.
type Route struct {
	UpstreamID      int64
	UpstreamName    string
	UpstreamModelID string
	BaseURL         string
	APIKey          string
	Metadata        string // merged metadata (carries pricing for cost tracking)
}

// RouteForModel resolves a gateway model ID to its upstream via the registry.
// Disabled models are treated as unknown (ErrModelNotFound).
func (s *Store) RouteForModel(ctx context.Context, gatewayID string) (*Route, error) {
	var r Route
	err := s.db.QueryRowContext(ctx, `
		SELECT m.upstream_id, u.name, m.upstream_model_id, u.base_url, u.api_key, m.metadata
		FROM models m JOIN upstreams u ON u.id = m.upstream_id
		WHERE m.gateway_id = ? AND m.disabled = 0
		  AND u.disabled = 0 AND u.reachable = 1
		ORDER BY u.position ASC
		LIMIT 1`, gatewayID).
		Scan(&r.UpstreamID, &r.UpstreamName, &r.UpstreamModelID, &r.BaseURL, &r.APIKey, &r.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrModelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("route for model %q: %w", gatewayID, err)
	}
	return &r, nil
}

// UsageEvent is one proxied LLM call's token/cost accounting.
type UsageEvent struct {
	KeyID           int64
	UpstreamID      int64
	GatewayModel    string
	UpstreamModel   string
	PromptTokens    int
	CompletionToken int
	CachedTokens    int
	ReasoningTokens int
	CostUSD         *float64 // computed by the gateway (item: cost tracking)
	UpstreamCostUSD *float64 // reported by the upstream, when present
	RawUsage        string   // verbatim usage JSON
}

// UsageEvents returns all recorded usage events (test/admin access).
func (s *Store) UsageEvents(ctx context.Context) ([]UsageEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_id, upstream_id, gateway_model, upstream_model,
		       prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens,
		       cost_usd, upstream_cost_usd, raw_usage
		FROM usage_events ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageEvent
	for rows.Next() {
		var e UsageEvent
		var keyID sql.NullInt64
		var cost, ucost sql.NullFloat64
		if err := rows.Scan(&keyID, &e.UpstreamID, &e.GatewayModel, &e.UpstreamModel,
			&e.PromptTokens, &e.CompletionToken, &e.CachedTokens, &e.ReasoningTokens,
			&cost, &ucost, &e.RawUsage); err != nil {
			return nil, err
		}
		// key_id is NULL once the key is deleted (history is kept).
		e.KeyID = keyID.Int64
		if cost.Valid {
			v := cost.Float64
			e.CostUSD = &v
		}
		if ucost.Valid {
			v := ucost.Float64
			e.UpstreamCostUSD = &v
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UsageSummaryRow aggregates usage per model over the whole filter range.
// Key is not part of the grouping — the key filter already narrows the query,
// and callers that need the per-key split use the requests list.
type UsageSummaryRow struct {
	GatewayModel    string
	Requests        int
	PromptTokens    int
	CompletionToken int
	CachedTokens    int
	CostUSD         float64
	UpstreamCostUSD float64
}

// UsageFilter narrows a usage query. Zero-valued fields impose no
// constraint. From and To are inclusive bounds on the event timestamp and,
// when set, must be in the store's canonical UTC form (see FormatTime).
type UsageFilter struct {
	From string
	To   string
	Keys []string // key names; empty means every key
	// IncludeDeleted also matches events whose key has been deleted (a NULL
	// key join). Combined with Keys it is a union; on its own it matches only
	// deleted-key events.
	IncludeDeleted bool
}

// where renders the filter as SQL conditions and their arguments.
func (f UsageFilter) where(tsCol, keyCol string) ([]string, []any) {
	var conds []string
	var args []any
	if f.From != "" {
		conds = append(conds, tsCol+" >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, tsCol+" <= ?")
		args = append(args, f.To)
	}
	if len(f.Keys) > 0 || f.IncludeDeleted {
		var parts []string
		if len(f.Keys) > 0 {
			qmarks := strings.TrimSuffix(strings.Repeat("?,", len(f.Keys)), ",")
			parts = append(parts, keyCol+" IN ("+qmarks+")")
			for _, k := range f.Keys {
				args = append(args, k)
			}
		}
		if f.IncludeDeleted {
			parts = append(parts, keyCol+" IS NULL")
		}
		conds = append(conds, "("+strings.Join(parts, " OR ")+")")
	}
	return conds, args
}

// UsageSummary aggregates recorded usage per model over the filter range, for
// the admin dashboard, optionally narrowed by f. The LEFT JOIN keeps the key
// filter (vk.name) usable without grouping by key.
func (s *Store) UsageSummary(ctx context.Context, f UsageFilter) ([]UsageSummaryRow, error) {
	conds, args := f.where("ue.created_at", "vk.name")
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT ue.gateway_model,
		       COUNT(*), SUM(ue.prompt_tokens), SUM(ue.completion_tokens),
		       SUM(ue.cached_tokens),
		       COALESCE(SUM(ue.cost_usd), 0), COALESCE(SUM(ue.upstream_cost_usd), 0)
		FROM usage_events ue
		LEFT JOIN virtual_keys vk ON vk.id = ue.key_id
		`+where+`
		GROUP BY ue.gateway_model
		ORDER BY ue.gateway_model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageSummaryRow
	for rows.Next() {
		var r UsageSummaryRow
		if err := rows.Scan(&r.GatewayModel, &r.Requests,
			&r.PromptTokens, &r.CompletionToken, &r.CachedTokens,
			&r.CostUSD, &r.UpstreamCostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordUsage persists a usage event. Failures are logged by callers; they
// must never fail the client's request.
func (s *Store) RecordUsage(ctx context.Context, e UsageEvent) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_events
			(key_id, upstream_id, gateway_model, upstream_model,
			 prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens,
			 cost_usd, upstream_cost_usd, raw_usage)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.KeyID, e.UpstreamID, e.GatewayModel, e.UpstreamModel,
		e.PromptTokens, e.CompletionToken, e.CachedTokens, e.ReasoningTokens,
		e.CostUSD, e.UpstreamCostUSD, e.RawUsage)
	if err != nil {
		return fmt.Errorf("record usage: %w", err)
	}
	return nil
}
