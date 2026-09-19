import type { FilterState } from "react-data-table-component";
import { useCallback, useEffect, useRef, useState } from "react";
import type { ColumnFilters, FilterOp, ListQuery } from "./types";

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

// mapOp maps the library's text-filter operator ids onto the API ops.
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
    default:
      return "contains";
  }
}

// translateFilters converts the library's per-column filter state into the
// API's ColumnFilters. A set filter (filterType "set") carries values and
// becomes an "in" membership; a text filter uses its operator and first
// condition's value.
function translateFilters(
  values: Record<string, FilterState>,
): ColumnFilters {
  const out: ColumnFilters = {};
  for (const [id, f] of Object.entries(values)) {
    if (f.values) {
      out[id] = { op: "in", values: f.values };
      continue;
    }
    const c = f.condition1;
    if (!c) continue;
    const op = mapOp(c.operator);
    if (op === "blank" || op === "notBlank") {
      out[id] = { op, values: [] };
      continue;
    }
    if (c.value === undefined || c.value === "") continue;
    out[id] = { op, values: [String(c.value)] };
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
