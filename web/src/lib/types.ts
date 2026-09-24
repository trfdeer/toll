// Shared types for the toll admin API. These mirror the JSON shapes produced
// by internal/admin/admin.go; keep them in sync when the Go handlers change.
// Migrated resources (virtual keys) use the generated types in gen/ instead.

// ---- profiles ----

// FilterMode constrains a profile along one dimension.
export type FilterMode = "none" | "include" | "exclude";

// KeyFilter is a provider or model constraint. "none" allows everything,
// "include" allows only Values, "exclude" allows everything but Values.
export interface KeyFilter {
  mode: FilterMode;
  values: string[];
}

// ---- profiles ----

// Profile is a named, reusable model-access rule shared by virtual keys. It
// is either a leaf (its own provider/model filters, no parents) or derived
// (the union of its parents' sets, no filters of its own). Its set of allowed
// models resolves live against the registry, so models discovered later are
// picked up automatically.
export interface Profile {
  name: string;
  providerFilter: KeyFilter;
  modelFilter: KeyFilter;
  /** Names of the profiles a derived profile unions; empty for a leaf. */
  parents: string[];
  /** The seeded "All" profile is read-only. */
  isDefault: boolean;
  /** Number of virtual keys referencing this profile. */
  keyCount: number;
  /** Number of profiles that inherit from this one. */
  childCount: number;
}

export interface ProfilesResponse {
  profiles: Profile[];
  total: number;
}

export interface ProfileRequest {
  name: string;
  providerFilter: KeyFilter;
  modelFilter: KeyFilter;
  parents: string[];
}

// ---- usage ----

export interface UsageRow {
  gatewayModel: string;
  requests: number;
  promptTokens: number;
  cachedTokens: number;
  completionTokens: number;
  costUSD: number;
}

export interface UsageTotals {
  requests: number;
  promptTokens: number;
  cachedTokens: number;
  completionTokens: number;
  costUSD: number;
}

export interface UsageSummary {
  rows: UsageRow[];
  /** Number of grouped rows matching the filter (for pagination). */
  total: number;
  /** Aggregate over the whole filtered set, not just the current page.
   * (The old totalReqs/totalCost duplicates are gone — proto/README item 9.) */
  totals: UsageTotals;
}

export interface RequestRow {
  id: number;
  conversationId: string;
  keyName: string;
  gatewayModel: string;
  status: number;
  promptTokens: number;
  cachedTokens: number;
  completionTokens: number;
  costUSD: number | null;
  createdAt: string;
  /** Wall-clock duration in milliseconds; null while still in flight. */
  durationMs: number | null;
}

export interface RequestsResponse {
  requests: RequestRow[];
  total: number;
}

// ChatMessage is one turn in a request's conversation, normalized by the
// server for rendering.
export interface ToolCall {
  id?: string;
  name: string;
  arguments?: string;
}

export interface ChatMessage {
  role: string;
  content: string;
  reasoning?: string;
  name?: string;
  toolCallId?: string;
  toolCalls?: ToolCall[];
  finishReason?: string;
}

// RequestDetailView is one transcript normalized into a conversation.
export interface RequestDetailView {
  id: number;
  conversationId: string;
  gatewayModel: string;
  upstreamModel: string;
  status: number;
  createdAt: string;
  completedAt: string;
  durationMs: number | null;
  /** False when prompt storage is disabled: messages will be empty. */
  contentStored: boolean;
  messages: ChatMessage[];
}

// ---- query parameters ----

export type QueryValue =
  | string
  | number
  | boolean
  | readonly string[]
  | null
  | undefined;

export type QueryParams = Record<string, QueryValue>;

// UsageQuery is the shared from/to/key narrowing accepted by the usage and
// requests calls. The UI's "(deleted keys)" pseudo-entry maps onto
// includeDeletedKeys; real key names ride keys.
export interface UsageQuery {
  /** Inclusive lower bound as an RFC3339 timestamp; null is unbounded. */
  from?: string | null;
  /** Inclusive upper bound as an RFC3339 timestamp; null is unbounded. */
  to?: string | null;
  keys?: readonly string[];
  includeDeletedKeys?: boolean;
}

// ---- list parameters ----

// FilterOp is how a server-side column filter matches values. The text ops
// mirror the library's filter operators; "in" is a set membership; the
// numeric ops compare as REAL (before/after on dates map onto lt/gt).
export type FilterOp =
  | "in"
  | "contains"
  | "notContains"
  | "equals"
  | "notEquals"
  | "startsWith"
  | "endsWith"
  | "blank"
  | "notBlank"
  | "gt"
  | "gte"
  | "lt"
  | "lte"
  | "between";

// FilterCondition is one condition: the values are ORed. BLANK/NOT_BLANK
// carry none; BETWEEN reads [lower, upper], either blank for unbounded.
export interface FilterCondition {
  op: FilterOp;
  values: string[];
}

// ColumnFilter is one column's predicate. The conditions combine per join;
// a single condition ignores it.
export interface ColumnFilterSpec {
  conditions: FilterCondition[];
  join?: "and" | "or";
}

// ColumnFilters maps a column id to its filter; columns AND together.
export type ColumnFilters = Record<string, ColumnFilterSpec>;

// ListQuery is the pagination, sorting and filtering shared by every admin
// list endpoint. `filters` is encoded to the API's JSON `filter` parameter.
export type ListQuery = {
  /** Page size; 0 means every row. Omit for the endpoint default. */
  limit?: number;
  offset?: number;
  sort?: string;
  dir?: "asc" | "desc";
  filters?: ColumnFilters;
};

// ---- errors ----

/** ApiError is the error body returned by the admin API on failure. */
export interface ApiError {
  error: string;
}
