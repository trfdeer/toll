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
import type { FormEvent, ReactNode } from "react";
import { useEffect, useRef, useState } from "react";
import PageState from "../components/PageState";
import RequestDetailPanel from "../components/RequestDetail";
import StructuredTable from "../components/StructuredTable";
import { getKeys, getRequest, getRequests, getUsage } from "../lib/api";
import { errorMessage } from "../lib/errors";
import { formatDuration } from "../lib/format";
import type { RequestDetail, RequestRow, UsageSummary } from "../lib/types";

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

export default function Usage() {
  const [summary, setSummary] = useState<UsageSummary | null>(null);
  const [requests, setRequests] = useState<RequestRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [keyOptions, setKeyOptions] = useState<string[]>([]);
  const [draft, setDraft] = useState<Filters>(defaultFilters);
  const [applied, setApplied] = useState<Filters>(defaultFilters);
  // autoRefresh is the poll interval in seconds; 0 disables it. tick just
  // re-triggers the fetch effect on each interval.
  const [autoRefresh, setAutoRefresh] = useState(0);
  const [tick, setTick] = useState(0);
  // refreshing/updatedAt drive the in-place status line: an auto-refresh keeps
  // the current table on screen while the new numbers are on the wire.
  const [refreshing, setRefreshing] = useState(false);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);
  // lastApplied lets the data effect tell a poll (same filters) from a new
  // query (Apply/Reset), which is the difference between swapping the table
  // and reloading the whole view.
  const lastApplied = useRef<Filters | null>(null);
  const [detail, setDetail] = useState<RequestDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState<string | null>(null);

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

  // Auto-refresh: while enabled, bump tick on the chosen interval. The data
  // effect below depends on tick, so it re-fetches with the applied filters.
  useEffect(() => {
    if (autoRefresh <= 0) return;
    const id = setInterval(() => setTick((t) => t + 1), autoRefresh * 1000);
    return () => clearInterval(id);
  }, [autoRefresh]);

  // Key filter options come from the full key list, not just what a filtered
  // result happens to contain.
  useEffect(() => {
    getKeys()
      .then((k) => setKeyOptions(k.keys.map((x) => x.name)))
      .catch((e: unknown) => setError(errorMessage(e)));
  }, []);

  // The API applies the filters; only the draft changes on edit, so a fetch
  // happens when the user applies (or resets) rather than on every keystroke.
  // tick is in the deps so auto-refresh re-runs the same query.
  useEffect(() => {
    const { dateRange, fromTime, toTime, selectedKeys } = applied;
    // applied is replaced with a new object on Apply/Reset, so an unchanged
    // identity means this is a poll: keep the rows on screen and swap them in
    // when the response lands.
    const isPoll = lastApplied.current === applied;
    lastApplied.current = applied;

    const [fromDayDate, toDayDate] = dateRange;
    const from = bound(fromDayDate, fromTime || "00:00");
    const to = bound(toDayDate, toTime || "23:59");

    if (!isPoll) {
      // A new query: the current rows are for different filters, so there is
      // nothing worth keeping.
      setSummary(null);
      setRequests(null);
    }
    setRefreshing(true);
    // A slow response must not land after a later one and replace fresher
    // rows, so the effect's cleanup marks it stale.
    let stale = false;
    Promise.all([
      getUsage({ from, to, key: selectedKeys }),
      getRequests({ from, to, key: selectedKeys }),
    ])
      .then(([usage, rq]) => {
        if (stale) return;
        setSummary(usage);
        setRequests(rq.requests);
        setUpdatedAt(new Date());
        setError(null);
      })
      .catch((e: unknown) => {
        if (!stale) setError(errorMessage(e));
      })
      .finally(() => {
        if (!stale) setRefreshing(false);
      });
    return () => {
      stale = true;
    };
  }, [applied, tick]);

  // A failed poll keeps the rows that are already on screen and reports the
  // error in the notification below; only a first load with nothing to show
  // replaces the view.
  if (error && (!summary || !requests)) return <PageState error={error} />;
  if (!summary || !requests) return <PageState />;

  const total = summary.rows.reduce(
    (acc, r) => ({
      requests: acc.requests + r.requests,
      promptTokens: acc.promptTokens + r.promptTokens,
      cachedTokens: acc.cachedTokens + r.cachedTokens,
      completionTokens: acc.completionTokens + r.completionTokens,
      costUSD: acc.costUSD + (r.costUSD ?? 0),
    }),
    {
      requests: 0,
      promptTokens: 0,
      cachedTokens: 0,
      completionTokens: 0,
      costUSD: 0,
    },
  );

  const summaryRows: ReactNode[][] = summary.rows.map((r) => [
    r.gatewayModel,
    r.requests,
    r.promptTokens,
    r.cachedTokens,
    r.completionTokens,
    r.costUSD,
  ]);
  const summaryFooter: ReactNode[] = [
    total.requests,
    total.promptTokens,
    total.cachedTokens,
    total.completionTokens,
    Number(total.costUSD.toFixed(4)),
  ];

  const requestRows: ReactNode[][] = requests.map((r) => [
    r.createdAt,
    r.keyName,
    r.gatewayModel,
    r.status,
    r.promptTokens,
    r.cachedTokens,
    r.completionTokens,
    r.costUSD ?? "—",
    formatDuration(r.durationMs),
  ]);

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
                <StructuredTable
                  headers={[
                    "Model",
                    "Requests",
                    "Prompt",
                    "Cached",
                    "Completion",
                    "Cost",
                  ]}
                  rows={summaryRows}
                  footer={summaryFooter}
                  searchable={false}
                  empty="No usage recorded for this filter."
                />
              </TabPanel>

              <TabPanel>
                <StructuredTable
                  headers={[
                    "Time",
                    "Key",
                    "Model",
                    "Status",
                    "Prompt",
                    "Cached",
                    "Completion",
                    "Cost",
                    "Duration",
                  ]}
                  rows={requestRows}
                  searchable={false}
                  onRowClick={(i) => {
                    const r = requests[i];
                    if (r) openDetail(r.id);
                  }}
                  empty="No requests recorded for this filter."
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
