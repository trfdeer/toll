import {
  Button,
  Column,
  DatePicker,
  DatePickerInput,
  Form,
  Grid,
  InlineLoading,
  InlineNotification,
  MultiSelect,
  Select,
  SelectItem,
  Stack,
  Tab,
  TabList,
  TabPanel,
  TabPanels,
  Tabs,
  TextInput,
} from "@carbon/react";
import type { TableColumn } from "react-data-table-component";
import type { FormEvent } from "react";
import { useEffect, useState } from "react";
import RequestDetailPanel from "../components/RequestDetail";
import Table from "../components/Table";
import { getKeys, getRequest, getRequests, getUsage } from "../lib/api";
import { errorMessage } from "../lib/errors";
import { formatDuration } from "../lib/format";
import type {
  RequestDetail,
  RequestRow,
  UsageRow,
  UsageTotals,
} from "../lib/types";
import {
  serverTableProps,
  useServerRows,
  type ServerTableQuery,
} from "../lib/useServerRows";

// ymd formats a Date as YYYY-MM-DD in local time.
function ymd(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

// bound turns a picked local day/time into a UTC RFC3339 instant, which is
// what the API compares against. Returns null when no day is selected.
function bound(day: Date | undefined, time: string): string | null {
  if (!day) return null;
  return new Date(`${ymd(day)}T${time}`).toISOString();
}

interface Filters {
  dateRange: Date[];
  fromTime: string;
  toTime: string;
  selectedKeys: string[];
}

// DELETED_KEYS is the reserved key-filter value that selects usage whose key
// was deleted. It matches the server's sentinel (internal/admin).
const DELETED_KEYS = "__deleted__";

// keyLabel renders the reserved deleted-keys entry distinctly from real names.
function keyLabel(key: string): string {
  return key === DELETED_KEYS ? "(deleted keys)" : key;
}

// defaultFilters is the initial (and reset) filter state: the past week,
// whole days, all keys. Bounding the range by default keeps the dashboard
// (and its queries) focused on recent activity.
function defaultFilters(): Filters {
  const to = new Date();
  const from = new Date();
  from.setDate(from.getDate() - 6); // today plus the previous six days
  return {
    dateRange: [from, to],
    fromTime: "00:00",
    toTime: "23:59",
    selectedKeys: [],
  };
}

// SummaryRow is a usage summary row with a stable table key.
type SummaryRow = UsageRow & { id: string };

export default function Usage() {
  const [error, setError] = useState<string | null>(null);
  const [keyOptions, setKeyOptions] = useState<string[]>([]);
  const [draft, setDraft] = useState<Filters>(defaultFilters);
  const [applied, setApplied] = useState<Filters>(defaultFilters);
  // autoRefresh is the poll interval in seconds; 0 disables it.
  const [autoRefresh, setAutoRefresh] = useState(0);
  // refreshing/updatedAt drive the in-place status line.
  const [refreshing, setRefreshing] = useState(false);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);
  const [detail, setDetail] = useState<RequestDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState<string | null>(null);

  // The applied date range/time and keys are the shared server filters for
  // both tables. Strings and the keys array keep a stable identity until the
  // user applies a new filter set.
  const [fromDay, toDay] = applied.dateRange;
  const from = bound(fromDay, applied.fromTime || "00:00");
  const to = bound(toDay, applied.toTime || "23:59");
  const keys = applied.selectedKeys;

  const fetchSummary = (q: ServerTableQuery) =>
    getUsage({ from, to, key: keys, ...q }).then((r) => ({
      rows: r.rows.map((row) => ({ ...row, id: row.gatewayModel })),
      total: r.total,
      meta: r.totals,
    }));
  const summaryTable = useServerRows<SummaryRow, UsageTotals>(fetchSummary, {
    deps: [from, to, keys],
    onError: (e) => setError(errorMessage(e)),
  });

  const fetchRequests = (q: ServerTableQuery) =>
    getRequests({ from, to, key: keys, ...q }).then((r) => ({
      rows: r.requests,
      total: r.total,
    }));
  const requestsTable = useServerRows<RequestRow>(fetchRequests, {
    deps: [from, to, keys],
    onError: (e) => setError(errorMessage(e)),
  });

  const { reload: reloadSummary } = summaryTable;
  const { reload: reloadRequests } = requestsTable;

  // openDetail loads one request's conversation into the side panel.
  const openDetail = (id: number) => {
    setDetail(null);
    setDetailError(null);
    setDetailLoading(true);
    getRequest(id)
      .then(setDetail)
      .catch((e: unknown) => setDetailError(errorMessage(e)))
      .finally(() => setDetailLoading(false));
  };

  const closeDetail = () => {
    setDetail(null);
    setDetailError(null);
    setDetailLoading(false);
  };

  const patch = (fields: Partial<Filters>) =>
    setDraft((d) => ({ ...d, ...fields }));

  const apply = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    setApplied(draft);
  };

  const reset = () => {
    const d = defaultFilters();
    setDraft(d);
    setApplied(d);
  };

  // Auto-refresh: while enabled, re-run both server queries on the interval.
  useEffect(() => {
    if (autoRefresh <= 0) return;
    const id = setInterval(() => {
      setRefreshing(true);
      reloadSummary();
      reloadRequests();
    }, autoRefresh * 1000);
    return () => clearInterval(id);
  }, [autoRefresh, reloadSummary, reloadRequests]);

  // Once both tables have settled, mark the status line as up to date.
  useEffect(() => {
    if (!summaryTable.loading && !requestsTable.loading) {
      setRefreshing(false);
      setUpdatedAt(new Date());
    }
  }, [summaryTable.loading, requestsTable.loading]);

  // Key filter options come from the full key list, not just what a filtered
  // result happens to contain.
  useEffect(() => {
    getKeys({ limit: 0 })
      .then((k) => setKeyOptions(k.keys.map((x) => x.name)))
      .catch((e: unknown) => setError(errorMessage(e)));
  }, []);

  const totals = summaryTable.meta;

  // The footer shows the true totals over the whole filtered set, which the
  // API computes independently of the current page.
  const summaryColumns: TableColumn<SummaryRow>[] = [
    {
      id: "model",
      name: "Model",
      selector: (r) => r.gatewayModel,
      sortable: true,
      filterable: true,
      footer: "Total",
    },
    {
      id: "requests",
      name: "Requests",
      selector: (r) => r.requests,
      sortable: true,
      right: true,
      footer: totals?.requests ?? "",
    },
    {
      id: "prompt",
      name: "Prompt",
      selector: (r) => r.promptTokens,
      sortable: true,
      right: true,
      footer: totals?.promptTokens ?? "",
    },
    {
      id: "cached",
      name: "Cached",
      selector: (r) => r.cachedTokens,
      sortable: true,
      right: true,
      footer: totals?.cachedTokens ?? "",
    },
    {
      id: "completion",
      name: "Completion",
      selector: (r) => r.completionTokens,
      sortable: true,
      right: true,
      footer: totals?.completionTokens ?? "",
    },
    {
      id: "cost",
      name: "Cost",
      selector: (r) => r.costUSD,
      sortable: true,
      right: true,
      footer: totals ? Number(totals.costUSD.toFixed(4)) : "",
    },
  ];

  const requestColumns: TableColumn<RequestRow>[] = [
    {
      id: "time",
      name: "Time",
      selector: (r) => r.createdAt,
      sortable: true,
    },
    {
      id: "key",
      name: "Key",
      selector: (r) => r.keyName,
      sortable: true,
      filterable: true,
    },
    {
      id: "model",
      name: "Model",
      selector: (r) => r.gatewayModel,
      sortable: true,
      filterable: true,
    },
    {
      id: "status",
      name: "Status",
      selector: (r) => r.status,
      sortable: true,
      right: true,
      filterable: true,
    },
    {
      id: "prompt",
      name: "Prompt",
      selector: (r) => r.promptTokens,
      sortable: true,
      right: true,
      filterable: true,
    },
    {
      id: "cached",
      name: "Cached",
      selector: (r) => r.cachedTokens,
      sortable: true,
      right: true,
      filterable: true,
    },
    {
      id: "completion",
      name: "Completion",
      selector: (r) => r.completionTokens,
      sortable: true,
      right: true,
      filterable: true,
      width: "150px",
      grow: 0,
    },
    {
      id: "cost",
      name: "Cost",
      selector: (r) => r.costUSD,
      format: (r) => r.costUSD ?? "—",
      sortable: true,
      right: true,
      filterable: true,
    },
    {
      id: "duration",
      name: "Duration",
      selector: (r) => r.durationMs,
      format: (r) => formatDuration(r.durationMs),
      sortable: true,
      right: true,
    },
  ];

  return (
    <Grid>
      <Column lg={{ span: 13, offset: 3 }}>
        <Stack gap={7}>
          <Form onSubmit={apply}>
            <Stack gap={5}>
              <Stack orientation="horizontal" gap={3}>
                <DatePicker
                  datePickerType="range"
                  dateFormat="Y-m-d"
                  value={draft.dateRange}
                  onChange={(dates) => patch({ dateRange: dates ?? [] })}
                >
                  <DatePickerInput
                    id="usage-from"
                    size="sm"
                    labelText="From"
                    placeholder="yyyy-mm-dd"
                  />
                  <DatePickerInput
                    id="usage-to"
                    size="sm"
                    labelText="To"
                    placeholder="yyyy-mm-dd"
                  />
                </DatePicker>
                <TextInput
                  id="usage-from-time"
                  size="sm"
                  type="time"
                  labelText="From time"
                  value={draft.fromTime}
                  onChange={(e) => patch({ fromTime: e.target.value })}
                />
                <TextInput
                  id="usage-to-time"
                  size="sm"
                  type="time"
                  labelText="To time"
                  value={draft.toTime}
                  onChange={(e) => patch({ toTime: e.target.value })}
                />
              </Stack>
              <MultiSelect
                id="usage-keys"
                size="sm"
                titleText="Keys"
                label="All keys"
                items={[...keyOptions, DELETED_KEYS]}
                selectedItems={draft.selectedKeys}
                itemToString={keyLabel}
                onChange={({ selectedItems }) =>
                  patch({ selectedKeys: selectedItems ?? [] })
                }
              />
              <Stack
                orientation="horizontal"
                gap={3}
                className="usage-form__actions"
              >
                <Select
                  id="usage-auto-refresh"
                  size="sm"
                  labelText="Auto-refresh"
                  value={String(autoRefresh)}
                  onChange={(e) => setAutoRefresh(Number(e.target.value))}
                >
                  <SelectItem value="0" text="Off" />
                  <SelectItem value="10" text="Every 10s" />
                  <SelectItem value="30" text="Every 30s" />
                  <SelectItem value="60" text="Every minute" />
                </Select>
                {refreshing ? (
                  <InlineLoading
                    className="usage-form__status"
                    status="active"
                    description="Updating…"
                  />
                ) : (
                  updatedAt && (
                    <span className="usage-form__status">
                      Updated {updatedAt.toLocaleTimeString()}
                    </span>
                  )
                )}
                <Button
                  type="button"
                  size="sm"
                  kind="secondary"
                  onClick={reset}
                >
                  Reset
                </Button>
                <Button type="submit" size="sm">
                  Apply
                </Button>
              </Stack>
            </Stack>
          </Form>

          {error && (
            <InlineNotification
              kind="error"
              lowContrast
              title="Something went wrong"
              subtitle={error}
              onCloseButtonClick={() => setError(null)}
            />
          )}

          <Tabs>
            <TabList aria-label="Usage">
              <Tab>Summary</Tab>
              <Tab>Requests</Tab>
            </TabList>
            <TabPanels>
              <TabPanel>
                <Table
                  columns={summaryColumns}
                  data={summaryTable.rows}
                  noDataComponent="No usage recorded for this filter."
                  {...serverTableProps(summaryTable)}
                />
              </TabPanel>

              <TabPanel>
                <Table
                  columns={requestColumns}
                  data={requestsTable.rows}
                  noDataComponent="No requests recorded for this filter."
                  highlightOnHover
                  pointerOnHover
                  onRowClicked={(r) => openDetail(r.id)}
                  {...serverTableProps(requestsTable)}
                />
              </TabPanel>
            </TabPanels>
          </Tabs>
        </Stack>
      </Column>

      {(detailLoading || detail !== null || detailError !== null) && (
        <RequestDetailPanel
          detail={detail}
          loading={detailLoading}
          error={detailError}
          onClose={closeDetail}
        />
      )}
    </Grid>
  );
}
