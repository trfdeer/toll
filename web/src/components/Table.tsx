import DataTable from "react-data-table-component";
import type { TableProps } from "react-data-table-component";
import { tollTableStyles, tollTableTheme, useTableColorMode } from "../lib/table";

// Table is react-data-table-component preconfigured with toll's Carbon theme.
// It accepts the same props as the library's DataTable; only the theme, the
// density and the pagination defaults are preset, so call sites still declare
// their own columns, data and features.
export default function Table<T>({
  dense = true,
  pagination = true,
  paginationPerPage = 10,
  paginationRowsPerPageOptions = [10, 20, 50],
  customStyles,
  ...rest
}: TableProps<T>) {
  const colorMode = useTableColorMode();
  return (
    <DataTable
      {...rest}
      theme={tollTableTheme}
      colorMode={colorMode}
      dense={dense}
      pagination={pagination}
      paginationPerPage={paginationPerPage}
      paginationRowsPerPageOptions={paginationRowsPerPageOptions}
      customStyles={
        customStyles ? { ...tollTableStyles, ...customStyles } : tollTableStyles
      }
    />
  );
}