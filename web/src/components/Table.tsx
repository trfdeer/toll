import DataTable from "react-data-table-component";
import type { TableProps } from "react-data-table-component";
import { useCallback, useState } from "react";
import { tollTableStyles, tollTableTheme, useTableColorMode } from "../lib/table";

// Column widths are persisted per table under this key prefix in localStorage,
// so resized columns survive navigation and reloads. Call sites opt in with a
// stable `storageKey`; the stored map is keyed by the columns' own ids.
const widthsKey = (storageKey: string) => `toll.table-widths.${storageKey}`;

function loadWidths(storageKey: string): Record<string, number> | undefined {
  try {
    const raw = localStorage.getItem(widthsKey(storageKey));
    return raw ? (JSON.parse(raw) as Record<string, number>) : undefined;
  } catch {
    return undefined;
  }
}

export interface TablePropsWithResize<T> extends TableProps<T> {
  // storageKey enables drag-to-resize width persistence for this table.
  storageKey?: string;
}

// Table is react-data-table-component preconfigured with toll's Carbon theme.
// It accepts the same props as the library's DataTable; only the theme, the
// density and the pagination defaults are preset, so call sites still declare
// their own columns, data and features. Columns are resizable by default;
// passing a `storageKey` persists the widths across sessions.
export default function Table<T>({
  dense = true,
  pagination = true,
  paginationPerPage = 10,
  paginationRowsPerPageOptions = [10, 20, 50],
  resizable = true,
  storageKey,
  customStyles,
  ...rest
}: TablePropsWithResize<T>) {
  const colorMode = useTableColorMode();
  // Hydrated once on mount; the table owns the live widths during drags.
  const [initialWidths] = useState(() =>
    storageKey ? loadWidths(storageKey) : undefined,
  );
  const onColumnResize = useCallback(
    (
      _columnId: string | number,
      _width: number,
      allWidths: Record<string | number, number>,
    ) => {
      if (!storageKey) return;
      try {
        localStorage.setItem(widthsKey(storageKey), JSON.stringify(allWidths));
      } catch {
        // Private-mode/blocked storage: resizing still works for the session.
      }
    },
    [storageKey],
  );
  return (
    <DataTable
      {...rest}
      theme={tollTableTheme}
      colorMode={colorMode}
      dense={dense}
      pagination={pagination}
      paginationPerPage={paginationPerPage}
      paginationRowsPerPageOptions={paginationRowsPerPageOptions}
      resizable={resizable}
      initialColumnWidths={initialWidths}
      onColumnResize={onColumnResize}
      customStyles={
        customStyles ? { ...tollTableStyles, ...customStyles } : tollTableStyles
      }
    />
  );
}
