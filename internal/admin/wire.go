// Wire mapping between the toll.admin.v1 proto messages and the store's
// query types (MIGRATION.md). Semantic validation of column filters (zero
// conditions, blank text-op values, missing joins, non-numeric bounds) lives
// in store.buildFilters so every surface enforces the same rules.
package admin

import (
	"fmt"

	adminv1 "github.com/trfdeer/toll/gen/toll/admin/v1"
	"github.com/trfdeer/toll/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// listParamsFromProto maps a proto ListParams onto the store's list query
// parameters.
func listParamsFromProto(p *adminv1.ListParams) (store.ListParams, error) {
	out := store.ListParams{}
	if p == nil {
		return out, nil
	}
	if p.Limit != nil {
		switch {
		case *p.Limit < 0, *p.Limit > 1000:
			return out, badListParam("limit must be 0..1000")
		case *p.Limit == 0:
			out.Limit = -1 // every row
		default:
			out.Limit = int(*p.Limit)
		}
	}
	if p.GetOffset() < 0 {
		return out, badListParam("offset must be >= 0")
	}
	out.Offset = int(p.GetOffset())
	out.Sort = p.GetSort()
	switch p.GetDir() {
	case adminv1.SortDirection_SORT_DIRECTION_ASC:
		out.Dir = "asc"
	case adminv1.SortDirection_SORT_DIRECTION_DESC:
		out.Dir = "desc"
	}
	for col, cf := range p.GetFilter() {
		if out.Filter == nil {
			out.Filter = make(map[string]store.ColumnFilter)
		}
		out.Filter[col] = columnFilterFromProto(cf)
	}
	return out, nil
}

// columnFilterFromProto maps one column's filter: conditions keep their
// multi-condition shape and join, which the store's filter engine now
// expresses directly.
func columnFilterFromProto(cf *adminv1.ColumnFilter) store.ColumnFilter {
	out := store.ColumnFilter{
		Conditions: make([]store.ColumnCondition, 0, len(cf.GetConditions())),
	}
	for _, c := range cf.GetConditions() {
		out.Conditions = append(out.Conditions, store.ColumnCondition{
			Op:     filterOpFromProto(c.GetOp()),
			Values: c.GetValues(),
		})
	}
	switch cf.GetJoin() {
	case adminv1.FilterJoin_FILTER_JOIN_AND:
		out.Join = "and"
	case adminv1.FilterJoin_FILTER_JOIN_OR:
		out.Join = "or"
	}
	return out
}

// filterOpFromProto maps the proto ops onto the store's. UNSPECIFIED is read
// as IN (proto3 JSON omits zero-valued enums); the UI's date before/after
// operators map onto the numeric comparisons.
func filterOpFromProto(op adminv1.FilterOp) store.FilterOp {
	switch op {
	case adminv1.FilterOp_FILTER_OP_CONTAINS:
		return store.FilterContains
	case adminv1.FilterOp_FILTER_OP_NOT_CONTAINS:
		return store.FilterNotContains
	case adminv1.FilterOp_FILTER_OP_EQUALS:
		return store.FilterEq
	case adminv1.FilterOp_FILTER_OP_NOT_EQUALS:
		return store.FilterNotEq
	case adminv1.FilterOp_FILTER_OP_STARTS_WITH:
		return store.FilterStartsWith
	case adminv1.FilterOp_FILTER_OP_ENDS_WITH:
		return store.FilterEndsWith
	case adminv1.FilterOp_FILTER_OP_BLANK:
		return store.FilterBlank
	case adminv1.FilterOp_FILTER_OP_NOT_BLANK:
		return store.FilterNotBlank
	case adminv1.FilterOp_FILTER_OP_GT:
		return store.FilterGT
	case adminv1.FilterOp_FILTER_OP_GTE:
		return store.FilterGTE
	case adminv1.FilterOp_FILTER_OP_LT:
		return store.FilterLT
	case adminv1.FilterOp_FILTER_OP_LTE:
		return store.FilterLTE
	case adminv1.FilterOp_FILTER_OP_BETWEEN:
		return store.FilterBetween
	default:
		// UNSPECIFIED and IN share the set-membership semantics.
		return store.FilterIn
	}
}

// usageFilterFromProto maps the shared usage narrowing. Both bounds are
// inclusive (proto semantics match the store's >= / <= comparisons);
// timestamps convert to the store's canonical UTC form.
func usageFilterFromProto(f *adminv1.UsageFilter) store.UsageFilter {
	out := store.UsageFilter{Keys: f.GetKeys(), IncludeDeleted: f.GetIncludeDeletedKeys()}
	for _, bound := range []struct {
		ts  *timestamppb.Timestamp
		dst *string
	}{{f.GetFrom(), &out.From}, {f.GetTo(), &out.To}} {
		if bound.ts == nil || !bound.ts.IsValid() {
			continue
		}
		*bound.dst = store.FormatTime(bound.ts.AsTime())
	}
	return out
}

// badListParam marks client-caused list parameter failures so connectError
// maps them to invalid_argument.
func badListParam(msg string) error {
	return fmt.Errorf("%w: %s", store.ErrBadListParam, msg)
}
