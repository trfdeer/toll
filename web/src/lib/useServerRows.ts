import type { FilterState } from "react-data-table-component";
import { useCallback, useEffect, useRef, useState } from "react";
import type {
  ColumnFilters,
  FilterCondition,
  FilterOp,
  ListQuery,
} from "./types";

/** The query a view's fetcher receives. */
export interface ServerTableQuery extends ListQuery {
  limit: number;
  offset: number;
}

export interface ServerRows<T, M = undefined> {
  rows: T[];
  total: number;
  /** Optional extra payload from the fetcher (e.g. aggregate totals). */
  meta: M | undefined;
  loading: boolean;
  page: number;
  perPage: number;
  filterValues: Record<string, FilterState>;
  onPageChange: (page: number) => void;
  onRowsPerPageChange: (perPage: number) => void;
  onSort: (
    column: { id?: string | number },
    direction?: string | null,
  ) => void;
  onFilterChange: (columnId: string | number, filter: FilterState) => void;
  /** Re-run the current query (after a mutation). */
  reload: () => void;
}

// mapOp maps the library's filter operator ids onto the API ops. The date
// bounds before/after map onto the numeric comparisons (ISO strings sort
// chronologically, but the numeric columns are REAL comparisons).
function mapOp(operator: string | undefined): FilterOp {
  switch (operator) {
    case "equals":
      return "equals";
    case "notEquals":
    case "differsFrom":
      return "notEquals";
    case "startsWith":
      return "startsWith";
    case "endsWith":
      return "endsWith";
    case "blank":
    case "empty":
      return "blank";
    case "notBlank":
      return "notBlank";
    case "notContains":
      return "notContains";
    case "gt":
    case "after":
      return "gt";
    case "gte":
      return "gte";
    case "lt":
    case "before":
      return "lt";
    case "lte":
      return "lte";
    case "between":
      return "between";
    default:
      return "contains";
  }
}

// conditionOf converts one library filter condition; null drops it (blank
// values would be an invalid server filter, so they are not sent).
function conditionOf(c: FilterState["condition2"]): FilterCondition | null {
  if (!c) return null;
  const op = mapOp(c.operator);
  if (op === "blank" || op === "notBlank") {
    return { op, values: [] };
  }
  if (op === "between") {
    const values = [c.value ?? "", c.value2 ?? ""];
    if (values.every((v) => v === "")) return null;
    return { op, values };
  }
  if (c.value === undefined || c.value === "") return null;
  return { op, values: [String(c.value)] };
}

// translateFilters converts the library's per-column filter state into the
// API's multi-condition ColumnFilters: a set filter (filterType "set")
// carries values and becomes one "in" condition; a text/number filter maps
// its condition1 (and condition2, per the AND/OR toggle) so the UI's
// "contains a AND contains b" survives the trip to the server — today's
// dropped-condition2 bug is fixed here.
function translateFilters(
  values: Record<string, FilterState>,
): ColumnFilters {
  const out: ColumnFilters = {};
  for (const [id, f] of Object.entries(values)) {
    if (f.values) {
      out[id] = { conditions: [{ op: "in", values: f.values }] };
      continue;
    }
    const c1 = conditionOf(f.condition1);
    if (!c1) continue;
    const c2 = conditionOf(f.condition2);
    if (c2) {
      out[id] = {
        conditions: [c1, c2],
        join: f.logic === "OR" ? "or" : "and",
      };
    } else {
      out[id] = { conditions: [c1] };
    }
  }
  return out;
}

const EMPTY_FILTERS: Record<string, FilterState> = {};

/**
 * useServerRows drives a react-data-table-component table from the server:
 * the page, page size, sort column and per-column filters all become API query
 * parameters, and the row total comes from the response. Spread the returned
 * handlers onto the table's server-mode props.
 *
 * `deps` must have a fixed length across renders (e.g. the view's own filter
 * values); it re-runs the query when those change.
 */
export function useServerRows<T, M = undefined>(
  fetcher: (q: ServerTableQuery) => Promise<{ rows: T[]; total: number; meta?: M }>,
  options?: {
    perPage?: number;
    initialSort?: { column: string; dir: "asc" | "desc" };
    onError?: (err: unknown) => void;
    deps?: readonly unknown[];
  },
): ServerRows<T, M> {
  const [page, setPage] = useState(1);
  const [perPage, setPerPage] = useState(options?.perPage ?? 10);
  const [sort, setSort] = useState<{
    column: string;
    dir: "asc" | "desc";
  } | null>(options?.initialSort ?? null);
  const [filterValues, setFilterValues] =
    useState<Record<string, FilterState>>(EMPTY_FILTERS);
  const [rows, setRows] = useState<T[]>([]);
  const [total, setTotal] = useState(0);
  const [meta, setMeta] = useState<M | undefined>(undefined);
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);

  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const onErrorRef = useRef(options?.onError);
  onErrorRef.current = options?.onError;
  const deps = options?.deps ?? [];

  useEffect(() => {
    let stale = false;
    setLoading(true);
    const filters = translateFilters(filterValues);
    fetcherRef
      .current({
        limit: perPage,
        offset: (page - 1) * perPage,
        sort: sort?.column,
        dir: sort?.dir,
        filters: Object.keys(filters).length > 0 ? filters : undefined,
      })
      .then((res) => {
        if (stale) return;
        setRows(res.rows);
        setTotal(res.total);
        setMeta(res.meta);
        // Keep the controlled page in range when the total shrinks.
        const last = Math.max(1, Math.ceil(res.total / perPage));
        setPage((cur) => (cur > last ? last : cur));
      })
      .catch((e: unknown) => {
        if (!stale) onErrorRef.current?.(e);
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
    };
    // The query re-runs on page/sort/filter changes and the caller's deps.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page, perPage, sort, filterValues, nonce, ...deps]);

  const onPageChange = useCallback((p: number) => setPage(p), []);
  const onRowsPerPageChange = useCallback((ps: number) => {
    setPerPage(ps);
    setPage(1);
  }, []);
  const onSort = useCallback(
    (column: { id?: string | number }, direction?: string | null) => {
      setSort(
        direction
          ? { column: String(column.id), dir: direction === "asc" ? "asc" : "desc" }
          : null,
      );
      setPage(1);
    },
    [],
  );
  const onFilterChange = useCallback(
    (columnId: string | number, filter: FilterState) => {
      setFilterValues((prev) => ({ ...prev, [String(columnId)]: filter }));
      setPage(1);
    },
    [],
  );
  const reload = useCallback(() => setNonce((n) => n + 1), []);

  return {
    rows,
    total,
    meta,
    loading,
    page,
    perPage,
    filterValues,
    onPageChange,
    onRowsPerPageChange,
    onSort,
    onFilterChange,
    reload,
  };
}

/**
 * serverTableProps returns the react-data-table-component props that switch a
 * table into server mode, driven by a useServerRows result. Spread it onto the
 * table together with the column definitions.
 */
export function serverTableProps<T, M>(
  t: ServerRows<T, M>,
  opts: { defaultSortFieldId?: string; defaultSortAsc?: boolean } = {},
) {
  return {
    paginationServer: true,
    paginationTotalRows: t.total,
    paginationPage: t.page,
    onChangePage: t.onPageChange,
    paginationPerPage: t.perPage,
    onChangeRowsPerPage: t.onRowsPerPageChange,
    paginationRowsPerPageOptions: [10, 20, 50],
    sortServer: true,
    onSort: t.onSort,
    defaultSortFieldId: opts.defaultSortFieldId,
    defaultSortAsc: opts.defaultSortAsc,
    filterServer: true,
    filterValues: t.filterValues,
    onFilterChange: t.onFilterChange,
    progressPending: t.loading,
  };
}
