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
import "@carbon/charts-react/styles.css";
import { ScaleTypes, StackedAreaChart } from "@carbon/charts-react";
import type { TableColumn } from "react-data-table-component";
import type { FormEvent } from "react";
import { useEffect, useMemo, useState } from "react";
import RequestDetailPanel from "../components/RequestDetail";
import Table from "../components/Table";
import {
  getFilterValues,
  getRequest,
  getRequests,
  getUsage,
  getUsageSeries,
  type SeriesGrouping,
  type SeriesPointView,
  type SeriesView,
  type UsageSeriesView,
} from "../lib/api";
import { errorMessage } from "../lib/errors";
import { formatDuration } from "../lib/format";
import type {
  RequestDetailView,
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

// DELETED_KEYS is the UI's pseudo-entry for usage whose key was deleted; the
// API layer maps it onto UsageFilter.include_deleted_keys (the old reserved
// `__deleted__` query sentinel is gone).
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

// Trend chart controls: the breakdown dimension, the bucket width and the
// plotted metric. The metric is a pure reshape of the same series payload,
// so only grouping/bucket changes refetch.
const GROUPING_OPTIONS: ReadonlyArray<{ value: SeriesGrouping; text: string }> = [
  { value: "none", text: "Total" },
  { value: "model", text: "By model" },
  { value: "key", text: "By key" },
  { value: "upstream", text: "By provider" },
];
const BUCKET_OPTIONS = [
  { value: "1800", text: "30 minutes" },
  { value: "3600", text: "1 hour" },
  { value: "86400", text: "1 day" },
] as const;
type TrendMetric = "requests" | "tokens" | "cost";
const METRIC_OPTIONS: ReadonlyArray<{ value: TrendMetric; text: string }> = [
  { value: "requests", text: "Requests" },
  { value: "tokens", text: "Tokens" },
  { value: "cost", text: "Cost (USD)" },
];

// trendGroupLabel renders a series label for the chart legend: the ungrouped
// series has no label, and a deleted key groups under the empty label.
function trendGroupLabel(label: string, grouping: SeriesGrouping): string {
  if (label === "") {
    return grouping === "key" ? "(deleted key)" : "All";
  }
  return label;
}

// trendValue picks the plotted metric out of a bucket.
function trendValue(p: SeriesPointView, metric: TrendMetric): number {
  switch (metric) {
    case "cost":
      return p.costUSD;
    case "tokens":
      return p.promptTokens + p.completionTokens;
    default:
      return p.requests;
  }
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
  const [detail, setDetail] = useState<RequestDetailView | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState<string | null>(null);
  // Trend chart state: grouping/bucket refetch; the metric is a reshape.
  const [trendGrouping, setTrendGrouping] = useState<SeriesGrouping>("model");
  const [trendBucket, setTrendBucket] = useState<"1800" | "3600" | "86400">("3600");
  const [trendMetric, setTrendMetric] = useState<TrendMetric>("requests");
  const [trend, setTrend] = useState<UsageSeriesView | null>(null);
  const [trendLoading, setTrendLoading] = useState(false);

  // The applied date range/time and keys are the shared server filters for
  // both tables. Strings and the keys array keep a stable identity until the
  // user applies a new filter set.
  const [fromDay, toDay] = applied.dateRange;
  const from = bound(fromDay, applied.fromTime || "00:00");
  const to = bound(toDay, applied.toTime || "23:59");
  // The "(deleted keys)" pseudo-entry narrows via includeDeletedKeys; real
  // names ride the keys list.
  const keys = applied.selectedKeys.filter((k) => k !== DELETED_KEYS);
  const includeDeletedKeys = applied.selectedKeys.includes(DELETED_KEYS);
  // keysKey is the stable dep form of keys: the filtered array is rebuilt on
  // every render, and an unstable dep re-runs each query effect every render
  // — a "maximum update depth" loop the moment Apply actually submits.
  const keysKey = keys.join("\u0000");

  const fetchSummary = (q: ServerTableQuery) =>
    getUsage({ from, to, keys, includeDeletedKeys, ...q }).then((r) => ({
      rows: r.rows.map((row) => ({ ...row, id: row.gatewayModel })),
      total: r.total,
      meta: r.totals,
    }));
  const summaryTable = useServerRows<SummaryRow, UsageTotals>(fetchSummary, {
    deps: [from, to, keysKey],
    onError: (e) => setError(errorMessage(e)),
  });

  const fetchRequests = (q: ServerTableQuery) =>
    getRequests({ from, to, keys, includeDeletedKeys, ...q }).then((r) => ({
      rows: r.requests,
      total: r.total,
    }));
  const requestsTable = useServerRows<RequestRow>(fetchRequests, {
    deps: [from, to, keysKey],
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

  // Key filter options come from the server's filter-value endpoint (the
  // live key names), not just what a filtered result happens to contain.
  useEffect(() => {
    getFilterValues("key")
      .then(setKeyOptions)
      .catch((e: unknown) => setError(errorMessage(e)));
  }, []);

  const totals = summaryTable.meta;

  // The trend chart shares the page's server filters; grouping and bucket
  // changes refetch, the metric choice is a pure reshape of the payload.
  useEffect(() => {
    let stale = false;
    setTrendLoading(true);
    getUsageSeries({
      from,
      to,
      keys,
      includeDeletedKeys,
      bucketSeconds: Number(trendBucket),
      groupBy: trendGrouping,
      topGroups: 8,
    })
      .then((r) => {
        if (!stale) setTrend(r);
      })
      .catch((e: unknown) => {
        if (!stale) setError(errorMessage(e));
      })
      .finally(() => {
        if (!stale) setTrendLoading(false);
      });
    return () => {
      stale = true;
    };
  }, [from, to, keysKey, includeDeletedKeys, trendGrouping, trendBucket]);

  // Carbon charts take long-format tabular data: one row per series point.
  const trendData = useMemo(() => {
    if (!trend) return [];
    return trend.series.flatMap((g: SeriesView) =>
      g.points.map((p) => ({
        date: new Date(p.t),
        group: trendGroupLabel(g.label, trendGrouping),
        value: trendValue(p, trendMetric),
      })),
    );
  }, [trend, trendMetric, trendGrouping]);

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
      // Deleted keys render empty server-side; the UI shows the placeholder.
      cell: (r) => r.keyName || "(deleted key)",
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
                    // Carbon's default input pattern is its m/d/Y locale
                    // format; with dateFormat="Y-m-d" it never matches, so
                    // the form is permanently invalid and Apply can never
                    // submit. Pin the pattern to the actual format.
                    pattern={String.raw`\d{4}-\d{2}-\d{2}`}
                  />
                  <DatePickerInput
                    id="usage-to"
                    size="sm"
                    labelText="To"
                    placeholder="yyyy-mm-dd"
                    pattern={String.raw`\d{4}-\d{2}-\d{2}`}
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
              <Tab>Trend</Tab>
            </TabList>
            <TabPanels>
              <TabPanel>
                <Table
                  columns={summaryColumns}
                  data={summaryTable.rows}
                  noDataComponent="No usage recorded for this filter."
                  storageKey="usage-summary"
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
                  storageKey="usage-requests"
                  onRowClicked={(r) => openDetail(r.id)}
                  {...serverTableProps(requestsTable)}
                />
              </TabPanel>

              <TabPanel>
                <Stack orientation="horizontal" gap={6}>
                  <Select
                    id="trend-grouping"
                    size="sm"
                    labelText="Group by"
                    value={trendGrouping}
                    onChange={(e: FormEvent<HTMLSelectElement>) =>
                      setTrendGrouping(
                        (e.target as HTMLSelectElement).value as SeriesGrouping,
                      )
                    }
                  >
                    {GROUPING_OPTIONS.map((o) => (
                      <SelectItem key={o.value} value={o.value} text={o.text} />
                    ))}
                  </Select>
                  <Select
                    id="trend-bucket"
                    size="sm"
                    labelText="Bucket"
                    value={trendBucket}
                    onChange={(e: FormEvent<HTMLSelectElement>) =>
                      setTrendBucket(
                        (e.target as HTMLSelectElement)
                          .value as typeof trendBucket,
                      )
                    }
                  >
                    {BUCKET_OPTIONS.map((o) => (
                      <SelectItem key={o.value} value={o.value} text={o.text} />
                    ))}
                  </Select>
                  <Select
                    id="trend-metric"
                    size="sm"
                    labelText="Metric"
                    value={trendMetric}
                    onChange={(e: FormEvent<HTMLSelectElement>) =>
                      setTrendMetric(
                        (e.target as HTMLSelectElement).value as TrendMetric,
                      )
                    }
                  >
                    {METRIC_OPTIONS.map((o) => (
                      <SelectItem key={o.value} value={o.value} text={o.text} />
                    ))}
                  </Select>
                  {trendLoading && <InlineLoading description="Loading…" />}
                </Stack>
                {trend !== null && trendData.length === 0 ? (
                  <p>No usage recorded for this filter.</p>
                ) : (
                  <StackedAreaChart
                    data={trendData}
                    options={{
                      axes: {
                        bottom: {
                          mapsTo: "date",
                          scaleType: ScaleTypes.TIME,
                        },
                        left: { mapsTo: "value" },
                      },
                      height: "400px",
                      legend: { position: "bottom" },
                    }}
                  />
                )}
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
