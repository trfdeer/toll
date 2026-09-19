package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrBadListParam marks an invalid sort or filter column/op, so the admin API
// can answer 400 rather than 500.
var ErrBadListParam = errors.New("invalid list parameter")

// FilterOp is how a server-side column filter matches a value.
type FilterOp string

const (
	// FilterIn keeps rows whose column equals any of Values (an OR). An empty
	// string in Values matches blank/NULL cells, mirroring the admin UI's
	// "(Blanks)" entry.
	FilterIn FilterOp = "in"
	// The remaining ops implement the admin UI's text-filter operators.
	FilterContains    FilterOp = "contains"
	FilterNotContains FilterOp = "notContains"
	FilterEq          FilterOp = "equals"
	FilterNotEq       FilterOp = "notEquals"
	FilterStartsWith  FilterOp = "startsWith"
	FilterEndsWith    FilterOp = "endsWith"
	FilterBlank       FilterOp = "blank"
	FilterNotBlank    FilterOp = "notBlank"
)

// FilterSpec is one column's filter as sent by the admin UI.
type FilterSpec struct {
	Op     FilterOp `json:"op"`
	Values []string `json:"values"`
}

// ListParams is the pagination, sorting and filtering shared by the admin
// list queries. The zero value means: endpoint default page size, natural
// order, no filters.
//
// Limit: >0 caps the page; 0 means the endpoint's default; <0 means every row
// (used by internal callers that need the full list). Note the admin HTTP
// layer maps a client `limit=0` to "-1" (every row), so a client and a store
// caller can mean different things by 0.
type ListParams struct {
	Limit  int
	Offset int
	Sort   string
	Dir    string
	Filter map[string]FilterSpec
}

// pageLimit resolves the effective row limit, clamping to max. It returns 0
// when the caller asked for every row (Limit < 0) or the resolved limit is
// non-positive, in which case callers omit the LIMIT clause.
func (p ListParams) pageLimit(def, max int) int {
	switch {
	case p.Limit < 0:
		return 0
	case p.Limit == 0:
		return def
	case p.Limit > max:
		return max
	default:
		return p.Limit
	}
}

// placeholders renders "?,?,?" for n arguments.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// buildFilters renders specs as SQL conditions. cols maps a column id to its
// SQL expression; an unknown id is an error, so the caller can answer 400.
// Specs are applied in sorted-key order so the generated SQL is deterministic.
func buildFilters(specs map[string]FilterSpec, cols map[string]string) ([]string, []any, error) {
	var conds []string
	var args []any
	keys := make([]string, 0, len(specs))
	for k := range specs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		spec := specs[key]
		expr, ok := cols[key]
		if !ok {
			return nil, nil, fmt.Errorf("%w: unknown filter column %q", ErrBadListParam, key)
		}
		switch spec.Op {
		case FilterIn, "":
			// Compare as text so numeric-derived columns (e.g. a metadata
			// limit) match the string values the UI sends.
			txt := "CAST(" + expr + " AS TEXT)"
			var vals []string
			blank := false
			for _, v := range spec.Values {
				if v == "" {
					blank = true
				} else {
					vals = append(vals, v)
				}
			}
			var parts []string
			if len(vals) > 0 {
				parts = append(parts, txt+" IN ("+placeholders(len(vals))+")")
				args = append(args, toAny(vals)...)
			}
			if blank {
				parts = append(parts, "("+expr+" IS NULL OR "+txt+" = '')")
			}
			if len(parts) == 0 {
				// A set filter with nothing selected matches no rows.
				parts = append(parts, "1=0")
			}
			conds = append(conds, "("+strings.Join(parts, " OR ")+")")
		case FilterBlank:
			conds = append(conds, "("+expr+" IS NULL OR CAST("+expr+" AS TEXT) = '')")
		case FilterNotBlank:
			conds = append(conds, "("+expr+" IS NOT NULL AND CAST("+expr+" AS TEXT) <> '')")
		case FilterContains, FilterNotContains, FilterEq, FilterNotEq, FilterStartsWith, FilterEndsWith:
			var parts []string
			for _, v := range spec.Values {
				v = strings.TrimSpace(v)
				if v == "" {
					continue
				}
				lower := strings.ToLower(v)
				esc := escapeLike(lower)
				switch spec.Op {
				case FilterContains:
					parts = append(parts, "LOWER("+expr+") LIKE ? ESCAPE '\\'")
					args = append(args, "%"+esc+"%")
				case FilterNotContains:
					parts = append(parts, "LOWER("+expr+") NOT LIKE ? ESCAPE '\\'")
					args = append(args, "%"+esc+"%")
				case FilterEq:
					parts = append(parts, "LOWER("+expr+") = ?")
					args = append(args, lower)
				case FilterNotEq:
					parts = append(parts, "LOWER("+expr+") <> ?")
					args = append(args, lower)
				case FilterStartsWith:
					parts = append(parts, "LOWER("+expr+") LIKE ? ESCAPE '\\'")
					args = append(args, esc+"%")
				case FilterEndsWith:
					parts = append(parts, "LOWER("+expr+") LIKE ? ESCAPE '\\'")
					args = append(args, "%"+esc)
				}
			}
			if len(parts) > 0 {
				conds = append(conds, "("+strings.Join(parts, " OR ")+")")
			}
		default:
			return nil, nil, fmt.Errorf("%w: unknown filter op %q", ErrBadListParam, spec.Op)
		}
	}
	return conds, args, nil
}

// buildOrder renders an ORDER BY for a list query. cols maps a column id to its
// SQL expression; an unknown id is an error. tiebreak is a column expression
// appended (with the same direction) so paging stays stable across ties.
func buildOrder(p ListParams, cols map[string]string, defKey, defDir, tiebreak string) (string, error) {
	key := p.Sort
	if key == "" {
		key = defKey
	}
	expr, ok := cols[key]
	if !ok {
		return "", fmt.Errorf("%w: unknown sort column %q", ErrBadListParam, key)
	}
	dir := strings.ToLower(p.Dir)
	if dir != "asc" && dir != "desc" {
		dir = defDir
	}
	order := " ORDER BY " + expr + " " + strings.ToUpper(dir)
	if tiebreak != "" {
		order += ", " + tiebreak + " " + strings.ToUpper(dir)
	}
	return order, nil
}

// escapeLike escapes the LIKE wildcards in a value so it matches literally.
// Callers pair it with an ESCAPE '\' clause.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}
