// Wire mapping between the toll.admin.v1 proto messages and the store's
// query types (MIGRATION.md phase 1). Multi-condition column filters and the
// numeric ops are rejected until the filter-engine work (MIGRATION.md 3c)
// extends store.FilterSpec; today's UI only ever sends single conditions.
package admin

import (
	"fmt"
	"strings"

	adminv1 "github.com/trfdeer/toll/gen/toll/admin/v1"
	"github.com/trfdeer/toll/internal/store"
)

// listParamsFromProto maps a proto ListParams onto the store's list query
// parameters, enforcing the validation rules documented in common.proto.
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
		spec, err := filterSpecFromProto(col, cf)
		if err != nil {
			return out, err
		}
		if out.Filter == nil {
			out.Filter = make(map[string]store.FilterSpec)
		}
		out.Filter[col] = spec
	}
	return out, nil
}

// filterSpecFromProto maps one column's filter. Only a single condition is
// accepted for now; the store expresses one op with ORed values per column.
func filterSpecFromProto(col string, cf *adminv1.ColumnFilter) (store.FilterSpec, error) {
	if len(cf.GetConditions()) == 0 {
		return store.FilterSpec{}, badListParam(fmt.Sprintf("filter %q: at least one condition is required", col))
	}
	if len(cf.GetConditions()) > 1 {
		return store.FilterSpec{}, badListParam(
			fmt.Sprintf("filter %q: multi-condition filters are not supported yet", col))
	}
	cond := cf.GetConditions()[0]
	op, err := filterOpFromProto(col, cond.GetOp())
	if err != nil {
		return store.FilterSpec{}, err
	}
	switch op {
	case store.FilterBlank, store.FilterNotBlank:
		return store.FilterSpec{Op: op}, nil
	}
	for _, v := range cond.GetValues() {
		if op != store.FilterIn && strings.TrimSpace(v) == "" {
			return store.FilterSpec{},
				badListParam(fmt.Sprintf("filter %q: op %s requires a non-blank value", col, op))
		}
	}
	return store.FilterSpec{Op: op, Values: cond.GetValues()}, nil
}

// filterOpFromProto maps the proto ops onto the store's. UNSPECIFIED is read
// as IN (proto3 JSON omits zero-valued enums).
func filterOpFromProto(col string, op adminv1.FilterOp) (store.FilterOp, error) {
	switch op {
	case adminv1.FilterOp_FILTER_OP_UNSPECIFIED, adminv1.FilterOp_FILTER_OP_IN:
		return store.FilterIn, nil
	case adminv1.FilterOp_FILTER_OP_CONTAINS:
		return store.FilterContains, nil
	case adminv1.FilterOp_FILTER_OP_NOT_CONTAINS:
		return store.FilterNotContains, nil
	case adminv1.FilterOp_FILTER_OP_EQUALS:
		return store.FilterEq, nil
	case adminv1.FilterOp_FILTER_OP_NOT_EQUALS:
		return store.FilterNotEq, nil
	case adminv1.FilterOp_FILTER_OP_STARTS_WITH:
		return store.FilterStartsWith, nil
	case adminv1.FilterOp_FILTER_OP_ENDS_WITH:
		return store.FilterEndsWith, nil
	case adminv1.FilterOp_FILTER_OP_BLANK:
		return store.FilterBlank, nil
	case adminv1.FilterOp_FILTER_OP_NOT_BLANK:
		return store.FilterNotBlank, nil
	default:
		return "", badListParam(fmt.Sprintf("filter %q: op is not supported yet", col))
	}
}

// badListParam marks client-caused list parameter failures so connectError
// maps them to invalid_argument.
func badListParam(msg string) error {
	return fmt.Errorf("%w: %s", store.ErrBadListParam, msg)
}
