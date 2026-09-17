import {
  DataTable,
  Pagination,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableHeader,
  TableRow,
  TableToolbar,
  TableToolbarContent,
  TableToolbarSearch,
} from "@carbon/react";
import type { ReactNode } from "react";
import { isValidElement, useState } from "react";

// cellText returns a comparable string for a cell, so columns that render
// React nodes (links, tags, buttons) still sort and filter by their text.
function cellText(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "bigint")
    return String(value);
  if (typeof value === "object" && isValidElement(value)) {
    const { children } = value.props as { children?: ReactNode };
    return Array.isArray(children)
      ? children.map(cellText).join(" ")
      : cellText(children);
  }
  return "";
}

// A cell value is any renderable node; rows are addressed by an id plus one
// property per column.
type CellValue = ReactNode;
type RowData = { id: string } & Record<string, CellValue>;

// sortRow keeps numeric columns in numeric order and everything else in
// locale order (numeric-aware, so "10" sorts after "9").
function sortRow(
  a: unknown,
  b: unknown,
  { sortDirection }: { sortDirection: string },
): number {
  const order = sortDirection === "ASC" ? 1 : -1;
  if (typeof a === "number" && typeof b === "number") return (a - b) * order;
  return (
    cellText(a).localeCompare(cellText(b), undefined, { numeric: true }) * order
  );
}

// The subset of Carbon's filter state this table relies on.
interface FilterRowsArgs {
  rowIds: string[];
  headers: Array<{ key: string }>;
  cellsById: Record<string, { value?: unknown } | undefined>;
  getCellId: (rowId: string, headerKey: string) => string;
  inputValue: string;
}

// filterRows matches the search input against every cell, including the text
// inside rendered React nodes. Carbon calls it with a single state object and
// expects the list of row ids that survive the filter.
function filterRows({
  rowIds,
  headers,
  cellsById,
  getCellId,
  inputValue,
}: FilterRowsArgs): string[] {
  const q = String(inputValue ?? "")
    .trim()
    .toLowerCase();
  if (!q) return rowIds;
  return rowIds.filter((rowId) =>
    headers.some((header) =>
      cellText(cellsById[getCellId(rowId, header.key)]?.value)
        .toLowerCase()
        .includes(q),
    ),
  );
}

export interface StructuredTableProps {
  /** Column headings, rendered as-is. */
  headers: ReactNode[];
  /** One array of cell values per row. */
  rows: CellValue[][];
  /** Placeholder shown when there are no rows. */
  empty: ReactNode;
  /** Optional values for a highlighted "Total" footer row. */
  footer?: CellValue[];
  /** Show the search box (default true). */
  searchable?: boolean;
  /** Optional node (usually a Button) rendered at the end of the toolbar. */
  actions?: ReactNode;
  /** When set, clicking a row calls this with the row's index. */
  onRowClick?: (index: number) => void;
  className?: string;
  pageSize?: number;
  pageSizes?: number[];
}

// Read-only table on Carbon's DataTable (compact rows). rows: array of
// cell-value arrays; empty renders the placeholder text instead. footer: an
// optional array of cell values rendered as a highlighted "Total" row whose
// leading label spans the remaining columns. Sorting and pagination are
// always on; search filtering is on unless searchable={false}. actions: an
// optional node (usually a Button) rendered at the end of the table toolbar.
export default function StructuredTable({
  headers,
  rows,
  empty,
  footer,
  searchable = true,
  actions,
  onRowClick,
  className,
  pageSize: initialPageSize = 10,
  pageSizes = [10, 20, 50],
}: StructuredTableProps) {
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(initialPageSize);

  // Even with no rows, render the toolbar so actions (e.g. "Add provider")
  // remain reachable. Search is omitted: there is nothing to filter yet.
  if (rows.length === 0) {
    return (
      <div
        className={["structured-table", className].filter(Boolean).join(" ")}
      >
        {actions && (
          <TableToolbar>
            <TableToolbarContent>{actions}</TableToolbarContent>
          </TableToolbar>
        )}
        <p>{empty}</p>
      </div>
    );
  }

  const cols = headers.map((h, i) => ({ key: `col${i}`, header: h }));
  const data: RowData[] = rows.map((cells, i) => ({
    id: `row-${i}`,
    ...Object.fromEntries(cells.map((v, j) => [`col${j}`, v] as const)),
  }));

  const toolbar = searchable || actions;

  return (
    <DataTable
      rows={data}
      headers={cols}
      size="sm"
      isSortable
      sortRow={sortRow}
      filterRows={filterRows}
    >
      {({
        rows: tableRows,
        headers: tableHeaders,
        getHeaderProps,
        getRowProps,
        getTableProps,
        onInputChange,
      }) => {
        const total = tableRows.length;
        const lastPage = Math.max(1, Math.ceil(total / pageSize));
        const current = Math.min(page, lastPage);
        const start = (current - 1) * pageSize;
        const visible = tableRows.slice(start, start + pageSize);

        return (
          <div
            className={["structured-table", className]
              .filter(Boolean)
              .join(" ")}
          >
            <TableContainer>
              {toolbar && (
                <TableToolbar>
                  <TableToolbarContent>
                    {searchable && (
                      <TableToolbarSearch size="sm" onChange={onInputChange} />
                    )}
                    {actions}
                  </TableToolbarContent>
                </TableToolbar>
              )}
              <Table {...getTableProps()} aria-label="data table">
                <TableHead>
                  <TableRow>
                    {tableHeaders.map((header) => (
                      <TableHeader
                        {...getHeaderProps({ header })}
                        key={header.key}
                      >
                        {header.header}
                      </TableHeader>
                    ))}
                  </TableRow>
                </TableHead>
                <TableBody>
                  {visible.map((row) => (
                    <TableRow
                      {...getRowProps({ row })}
                      key={row.id}
                      onClick={
                        onRowClick
                          ? () =>
                              onRowClick(Number(row.id.replace(/^row-/, "")))
                          : undefined
                      }
                      className={
                        onRowClick
                          ? "structured-table__row--clickable"
                          : undefined
                      }
                    >
                      {row.cells.map((cell) => (
                        <TableCell key={cell.id}>{cell.value}</TableCell>
                      ))}
                    </TableRow>
                  ))}
                  {footer && total > 0 && (
                    <TableRow className="structured-table__total">
                      <TableCell colSpan={tableHeaders.length - footer.length}>
                        Total
                      </TableCell>
                      {footer.map((v, i) => (
                        <TableCell key={i}>{v}</TableCell>
                      ))}
                    </TableRow>
                  )}
                </TableBody>
              </Table>
            </TableContainer>
            {total >= 10 && (
              <Pagination
                size="sm"
                totalItems={total}
                page={current}
                pageSize={pageSize}
                pageSizes={pageSizes}
                onChange={({ page: p, pageSize: ps }) => {
                  setPage(p);
                  setPageSize(ps);
                }}
              />
            )}
          </div>
        );
      }}
    </DataTable>
  );
}
