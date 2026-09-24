package store

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
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
	// The text ops implement the admin UI's text-filter operators.
	FilterContains    FilterOp = "contains"
	FilterNotContains FilterOp = "notContains"
	FilterEq          FilterOp = "equals"
	FilterNotEq       FilterOp = "notEquals"
	FilterStartsWith  FilterOp = "startsWith"
	FilterEndsWith    FilterOp = "endsWith"
	FilterBlank       FilterOp = "blank"
	FilterNotBlank    FilterOp = "notBlank"
	// The numeric ops compare CAST(col AS REAL); NULL cells never match.
	// before/after on timestamps map to GT/LT by the mapping layer.
	FilterGT      FilterOp = "gt"
	FilterGTE     FilterOp = "gte"
	FilterLT      FilterOp = "lt"
	FilterLTE     FilterOp = "lte"
	FilterBetween FilterOp = "between"
)

// ColumnCondition is one predicate within a column filter: the values are
// ORed. BLANK/NOT_BLANK ignore Values; BETWEEN reads Values[0] and Values[1]
// as the inclusive bounds, either of which may be blank for unbounded.
type ColumnCondition struct {
	Op     FilterOp
	Values []string
}

// ColumnFilter is one column's predicate. Conditions are combined with Join
// ("and"/"or"); a single condition ignores Join. Zero conditions and text ops
// with only blank values are invalid, as is an unspecified join on two or
// more conditions.
type ColumnFilter struct {
	Conditions []ColumnCondition
	Join       string
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
	Filter map[string]ColumnFilter
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

// buildFilters renders the column filters as SQL conditions. cols maps a
// column id to its SQL expression; an unknown id is an error, so the caller
// can answer 400. Filters apply in sorted-key order so the generated SQL is
// deterministic.
//
// Within a filter the conditions combine per Join (AND/OR); each condition's
// values OR. A column filter with zero conditions, a text op with only blank
// values, or two-plus conditions without an explicit join is ErrBadListParam
// (the proto documents these as invalid_argument, not silent match-all).
func buildFilters(specs map[string]ColumnFilter, cols map[string]string) ([]string, []any, error) {
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
		if len(spec.Conditions) == 0 {
			return nil, nil, fmt.Errorf("%w: filter %q has no conditions", ErrBadListParam, key)
		}
		if len(spec.Conditions) > 1 && spec.Join != "and" && spec.Join != "or" {
			return nil, nil, fmt.Errorf("%w: filter %q needs an explicit join", ErrBadListParam, key)
		}
		parts := make([]string, 0, len(spec.Conditions))
		for _, cond := range spec.Conditions {
			sql, condArgs, err := buildCondition(expr, cond)
			if err != nil {
				return nil, nil, fmt.Errorf("%w: filter %q: %s", ErrBadListParam, key, err.Error())
			}
			parts = append(parts, "("+sql+")")
			args = append(args, condArgs...)
		}
		joiner := " OR "
		if spec.Join == "and" {
			joiner = " AND "
		}
		conds = append(conds, "("+strings.Join(parts, joiner)+")")
	}
	return conds, args, nil
}

// buildCondition renders one condition. An empty SQL means the condition
// carries only blank values (an IN of just "" is still meaningful and is
// rendered); the caller turns an empty result into ErrBadListParam.
func buildCondition(expr string, cond ColumnCondition) (string, []any, error) {
	switch cond.Op {
	case FilterIn, "":
		// Compare as text so numeric-derived columns (e.g. a metadata
		// limit) match the string values the UI sends.
		txt := "CAST(" + expr + " AS TEXT)"
		var vals []string
		blank := false
		for _, v := range cond.Values {
			if v == "" {
				blank = true
			} else {
				vals = append(vals, v)
			}
		}
		var parts []string
		var args []any
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
		return strings.Join(parts, " OR "), args, nil
	case FilterBlank:
		return "(" + expr + " IS NULL OR CAST(" + expr + " AS TEXT) = '')", nil, nil
	case FilterNotBlank:
		return "(" + expr + " IS NOT NULL AND CAST(" + expr + " AS TEXT) <> '')", nil, nil
	case FilterContains, FilterNotContains, FilterEq, FilterNotEq, FilterStartsWith, FilterEndsWith:
		var parts []string
		var args []any
		for _, v := range cond.Values {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			lower := strings.ToLower(v)
			esc := escapeLike(lower)
			switch cond.Op {
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
		if len(parts) == 0 {
			// Blank-only text values are invalid, not a silent match-all.
			return "", nil, errors.New("op " + string(cond.Op) + " requires a non-blank value")
		}
		return strings.Join(parts, " OR "), args, nil
	case FilterGT, FilterGTE, FilterLT, FilterLTE:
		v, err := numericBound(cond.Values)
		if err != nil {
			return "", nil, err
		}
		op := map[FilterOp]string{
			FilterGT: ">", FilterGTE: ">=", FilterLT: "<", FilterLTE: "<=",
		}[cond.Op]
		// CAST keeps numeric semantics; NULL cells never match any
		// comparison, so a blank cell is excluded like the UI expects.
		// The bound binds as a float so a text bound can't compare as
		// "greater than every number".
		sql := "CAST(" + expr + " AS REAL) " + op + " ?"
		return sql, []any{v}, nil
	case FilterBetween:
		if len(cond.Values) == 0 || len(cond.Values) > 2 {
			return "", nil, errors.New("op between takes a lower and upper bound")
		}
		real := "CAST(" + expr + " AS REAL)"
		var parts []string
		var args []any
		for i, side := range []string{"lower", "upper"} {
			raw := strings.TrimSpace(cond.Values[i])
			if raw == "" {
				continue // unbounded side
			}
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return "", nil, fmt.Errorf("%s bound %q is not numeric", side, raw)
			}
			if side == "lower" {
				parts = append(parts, real+" >= ?")
			} else {
				parts = append(parts, real+" <= ?")
			}
			args = append(args, v)
		}
		if len(parts) == 0 {
			return "", nil, errors.New("op between requires a bound")
		}
		return strings.Join(parts, " AND "), args, nil
	default:
		return "", nil, fmt.Errorf("unknown filter op %q", cond.Op)
	}
}

// numericBound parses the single bound of a scalar numeric op.
func numericBound(values []string) (float64, error) {
	if len(values) != 1 {
		return 0, errors.New("op takes exactly one value")
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" {
		return 0, errors.New("op requires a value")
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("value %q is not numeric", raw)
	}
	return v, nil
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
