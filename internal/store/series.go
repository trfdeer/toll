package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// maxSeriesBuckets caps the dense zero-filled bucket grid: a pathological
// window/bucket combination (e.g. a year at one-minute buckets) must fail as
// a client error rather than materialize millions of points.
const maxSeriesBuckets = 10000

// SeriesPoint is one time bucket of aggregated usage.
type SeriesPoint struct {
	BucketStart      time.Time
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	ReasoningTokens  int64
	CostUSD          float64
}

// UsageSeriesGroup is one line/stack of a usage series chart.
type UsageSeriesGroup struct {
	// Label is the group value (model id, key name, upstream name); empty
	// for the single ungrouped series, "Other" for a top-groups remainder.
	Label  string
	Points []SeriesPoint
}

// SeriesOptions controls bucketing and grouping for UsageSeries.
type SeriesOptions struct {
	// Bucket is the width of each dense UTC-aligned bucket. Callers clamp it
	// to a sane range; zero or negative is an error.
	Bucket time.Duration
	// GroupBy is the breakdown dimension: "" (one ungrouped series), "model",
	// "key" or "upstream".
	GroupBy string
	// Top keeps only the N groups with the highest total cost, rolling the
	// rest into a trailing "Other" group; 0 keeps every group.
	Top int
	// Compare also computes the same-shaped series for the immediately
	// preceding window of equal length.
	Compare bool
}

// UsageSeriesResult is the full chart payload for a filtered window.
type UsageSeriesResult struct {
	Groups []UsageSeriesGroup
	Totals UsageTotals
	// Previous and PreviousTotals are populated only when SeriesOptions.Compare
	// was set; they cover the window of equal length immediately before the
	// resolved one. Groups use the current window's top-group labels so the
	// two windows' series correspond line for line.
	Previous       []UsageSeriesGroup
	PreviousTotals UsageTotals
}

// seriesGroupExpr returns the SQL expression and column label for a grouping
// dimension. The virtual-keys and upstreams joins are always present (the
// key filter needs the vk join anyway), so every expression is available.
func seriesGroupExpr(groupBy string) (string, error) {
	switch groupBy {
	case "":
		return "''", nil
	case "model":
		return "ue.gateway_model", nil
	case "key":
		// Deleted keys group under the empty label.
		return "COALESCE(vk.name, '')", nil
	case "upstream":
		return "COALESCE(u.name, '')", nil
	default:
		return "", fmt.Errorf("%w: unknown series grouping %q", ErrBadListParam, groupBy)
	}
}

// seriesRun computes the dense per-group points and the totals over the
// window implied by f. When f carries no time bounds, the window is derived
// from the data (MIN/MAX of the filtered set); an empty set yields no window
// and an empty result.
func (s *Store) seriesRun(ctx context.Context, f UsageFilter, opts SeriesOptions) (map[string][]SeriesPoint, UsageTotals, time.Time, time.Time, error) {
	if opts.Bucket < time.Second {
		return nil, UsageTotals{}, time.Time{}, time.Time{},
			fmt.Errorf("%w: bucket size must be at least one second", ErrBadListParam)
	}
	groupExpr, err := seriesGroupExpr(opts.GroupBy)
	if err != nil {
		return nil, UsageTotals{}, time.Time{}, time.Time{}, err
	}

	// Resolve the window. Time bounds come from the filter when present,
	// otherwise from the data (with the non-time constraints applied).
	tconds, targs := f.whereTime("ue.created_at")
	boundsFrom, boundsTo := f.From, f.To
	if boundsFrom == "" || boundsTo == "" {
		kconds, kargs := f.whereKeys("vk.name")
		conds := append(kconds, tconds...)
		args := append([]any{}, kargs...)
		args = append(args, targs...)
		where := ""
		if len(conds) > 0 {
			where = "WHERE " + strings.Join(conds, " AND ")
		}
		var minAt, maxAt sql.NullString
		if err := s.db.QueryRowContext(ctx, `
			SELECT MIN(ue.created_at), MAX(ue.created_at)
			FROM usage_events ue
			LEFT JOIN virtual_keys vk ON vk.id = ue.key_id
			`+where, args...).Scan(&minAt, &maxAt); err != nil {
			return nil, UsageTotals{}, time.Time{}, time.Time{},
				fmt.Errorf("series window: %w", err)
		}
		if !minAt.Valid || !maxAt.Valid {
			// No data in the filtered set: an empty series.
			return map[string][]SeriesPoint{}, UsageTotals{}, time.Time{}, time.Time{}, nil
		}
		if boundsFrom == "" {
			boundsFrom = minAt.String
		}
		if boundsTo == "" {
			boundsTo = maxAt.String
		}
	}
	windowStart, err := parseStoreTime(boundsFrom)
	if err != nil {
		return nil, UsageTotals{}, time.Time{}, time.Time{},
			fmt.Errorf("%w: bad lower bound", ErrBadListParam)
	}
	windowEnd, err := parseStoreTime(boundsTo)
	if err != nil {
		return nil, UsageTotals{}, time.Time{}, time.Time{},
			fmt.Errorf("%w: bad upper bound", ErrBadListParam)
	}
	if windowEnd.Before(windowStart) {
		return nil, UsageTotals{}, time.Time{}, time.Time{},
			fmt.Errorf("%w: upper bound precedes lower bound", ErrBadListParam)
	}

	// Bucket grid: UTC-aligned to whole multiples of the bucket width,
	// dense from the bucket containing windowStart through the bucket
	// containing windowEnd.
	bucketSecs := int64(opts.Bucket / time.Second)
	align := func(t time.Time) int64 {
		return (t.Unix() / bucketSecs) * bucketSecs
	}
	first, last := align(windowStart), align(windowEnd)
	buckets := int((last-first)/bucketSecs) + 1
	if buckets > maxSeriesBuckets {
		return nil, UsageTotals{}, time.Time{}, time.Time{},
			fmt.Errorf("%w: window spans %d buckets; raise the bucket size",
				ErrBadListParam, buckets)
	}

	conds, args := f.where("ue.created_at", "vk.name")
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT (CAST(strftime('%s', ue.created_at) AS INTEGER) / ?) * ? AS bucket,
		       `+groupExpr+`,
		       COUNT(*), COALESCE(SUM(ue.prompt_tokens), 0), COALESCE(SUM(ue.completion_tokens), 0),
		       COALESCE(SUM(ue.cached_tokens), 0), COALESCE(SUM(ue.reasoning_tokens), 0),
		       COALESCE(SUM(ue.cost_usd), 0)
		FROM usage_events ue
		LEFT JOIN virtual_keys vk ON vk.id = ue.key_id
		LEFT JOIN upstreams u ON u.id = ue.upstream_id
		`+where+`
		GROUP BY bucket, `+groupExpr, append([]any{bucketSecs, bucketSecs}, args...)...)
	if err != nil {
		return nil, UsageTotals{}, time.Time{}, time.Time{},
			fmt.Errorf("usage series: %w", err)
	}
	defer rows.Close()

	// Sparse aggregates per (bucket, group), folded into dense grids below.
	type agg struct {
		reqs, prompt, completion, cached, reasoning int64
		cost                                        float64
	}
	perGroup := map[string]map[int64]*agg{}
	var totals UsageTotals
	for rows.Next() {
		var bucket int64
		var label string
		var a agg
		if err := rows.Scan(&bucket, &label, &a.reqs, &a.prompt, &a.completion,
			&a.cached, &a.reasoning, &a.cost); err != nil {
			return nil, UsageTotals{}, time.Time{}, time.Time{}, err
		}
		g := perGroup[label]
		if g == nil {
			g = map[int64]*agg{}
			perGroup[label] = g
		}
		g[bucket] = &a
		totals.Requests += a.reqs
		totals.PromptTokens += a.prompt
		totals.CompletionTokens += a.completion
		totals.CachedTokens += a.cached
		totals.CostUSD += a.cost
	}
	if err := rows.Err(); err != nil {
		return nil, UsageTotals{}, time.Time{}, time.Time{}, err
	}

	// Dense zero-filled points per group.
	dense := make(map[string][]SeriesPoint, len(perGroup))
	for label, aggs := range perGroup {
		points := make([]SeriesPoint, buckets)
		for i := range points {
			points[i].BucketStart = time.Unix(first+int64(i)*bucketSecs, 0).UTC()
		}
		for bucket, a := range aggs {
			idx := int((bucket - first) / bucketSecs)
			p := &points[idx]
			p.Requests = a.reqs
			p.PromptTokens = a.prompt
			p.CompletionTokens = a.completion
			p.CachedTokens = a.cached
			p.ReasoningTokens = a.reasoning
			p.CostUSD = a.cost
		}
		dense[label] = points
	}
	return dense, totals, windowStart, windowEnd, nil
}

// seriesCost ranks groups by their total cost, descending.
func seriesCost(points []SeriesPoint) float64 {
	var sum float64
	for _, p := range points {
		sum += p.CostUSD
	}
	return sum
}

// selectTop orders the groups by total cost (descending) and, when top > 0
// and more groups exist, rolls the remainder into a trailing "Other" group.
// keepLabels, when non-nil, restricts the kept labels to that set (used to
// make a compared window's series correspond to the current one).
func selectTop(groups map[string][]SeriesPoint, top int, keepLabels []string) []UsageSeriesGroup {
	labels := make([]string, 0, len(groups))
	for label := range groups {
		labels = append(labels, label)
	}
	if keepLabels != nil {
		keep := make(map[string]bool, len(keepLabels))
		for _, l := range keepLabels {
			keep[l] = true
		}
		filtered := labels[:0]
		for _, l := range labels {
			if keep[l] {
				filtered = append(filtered, l)
			}
		}
		// Absorb dropped labels' points into "Other".
		var other []SeriesPoint
		for _, l := range labels {
			if !keep[l] {
				if other == nil {
					other = make([]SeriesPoint, len(groups[l]))
					for i, p := range groups[l] {
						other[i] = p
					}
				} else {
					for i, p := range groups[l] {
						other[i].Requests += p.Requests
						other[i].PromptTokens += p.PromptTokens
						other[i].CompletionTokens += p.CompletionTokens
						other[i].CachedTokens += p.CachedTokens
						other[i].ReasoningTokens += p.ReasoningTokens
						other[i].CostUSD += p.CostUSD
					}
				}
			}
		}
		labels = filtered
		if other != nil {
			labels = append(labels, "Other")
			groups["Other"] = other
		}
	}
	sort.Slice(labels, func(i, j int) bool {
		ci, cj := seriesCost(groups[labels[i]]), seriesCost(groups[labels[j]])
		if ci != cj {
			return ci > cj
		}
		return labels[i] < labels[j]
	})
	if top > 0 && len(labels) > top {
		var other []SeriesPoint
		for _, l := range labels[top:] {
			if other == nil {
				other = make([]SeriesPoint, len(groups[l]))
				copy(other, groups[l])
			} else {
				for i, p := range groups[l] {
					other[i].Requests += p.Requests
					other[i].PromptTokens += p.PromptTokens
					other[i].CompletionTokens += p.CompletionTokens
					other[i].CachedTokens += p.CachedTokens
					other[i].ReasoningTokens += p.ReasoningTokens
					other[i].CostUSD += p.CostUSD
				}
			}
		}
		kept := append([]string{}, labels[:top]...)
		labels = append(kept, "Other")
		groups["Other"] = other
	}
	out := make([]UsageSeriesGroup, 0, len(labels))
	for _, label := range labels {
		out = append(out, UsageSeriesGroup{Label: label, Points: groups[label]})
	}
	return out
}

// UsageSeries computes the dense, zero-filled usage time series for the
// filter window, optionally grouped, top-N-trimmed and compared against the
// preceding window of equal length. Buckets are UTC-aligned whole multiples
// of the bucket width.
func (s *Store) UsageSeries(ctx context.Context, f UsageFilter, opts SeriesOptions) (UsageSeriesResult, error) {
	dense, totals, windowStart, windowEnd, err := s.seriesRun(ctx, f, opts)
	if err != nil {
		return UsageSeriesResult{}, err
	}
	var result UsageSeriesResult
	if len(dense) == 0 {
		return result, nil
	}
	result.Totals = totals
	result.Groups = selectTop(dense, opts.Top, nil)

	if !opts.Compare {
		return result, nil
	}
	// The previous window is the equal-length window immediately before the
	// resolved one, with the same key narrowing but explicit bounds.
	span := windowEnd.Sub(windowStart)
	prev := f
	prev.From = FormatTime(windowStart.Add(-span))
	prev.To = FormatTime(windowEnd.Add(-span))
	prevDense, prevTotals, _, _, err := s.seriesRun(ctx, prev, opts)
	if err != nil {
		return UsageSeriesResult{}, err
	}
	result.PreviousTotals = prevTotals
	// Reuse the current window's group labels so the two series correspond;
	// everything else rolls into "Other" (or disappears when the previous
	// window is empty).
	keep := make([]string, 0, len(result.Groups))
	for _, g := range result.Groups {
		if g.Label != "Other" {
			keep = append(keep, g.Label)
		}
	}
	if len(prevDense) > 0 {
		result.Previous = selectTop(prevDense, 0, keep)
	}
	return result, nil
}

// parseStoreTime parses the store's canonical timestamp form.
func parseStoreTime(s string) (time.Time, error) {
	return time.Parse("2006-01-02T15:04:05.000Z", s)
}

// FilterValues returns the distinct values of a table filter column, for the
// admin UI's set-filter dropdowns: "model" is every gateway model seen in
// recorded usage, "key" every live virtual key name (deleted keys' names are
// not retained, so they cannot be offered). query is a case-insensitive
// substring narrowing for type-ahead; limit caps the result (0 → 100).
func (s *Store) FilterValues(ctx context.Context, column, query string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	like := "%" + escapeLike(query) + "%"
	var q string
	switch column {
	case "model":
		q = `SELECT DISTINCT ue.gateway_model FROM usage_events ue
			WHERE ue.gateway_model LIKE ? ESCAPE '\' ORDER BY ue.gateway_model LIMIT ?`
	case "key":
		q = `SELECT vk.name FROM virtual_keys vk
			WHERE vk.name LIKE ? ESCAPE '\' ORDER BY vk.name LIMIT ?`
	default:
		return nil, fmt.Errorf("%w: unknown filter column %q", ErrBadListParam, column)
	}
	rows, err := s.db.QueryContext(ctx, q, like, limit)
	if err != nil {
		return nil, fmt.Errorf("filter values %q: %w", column, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
