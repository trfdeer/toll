// Shared types for the toll admin API. These mirror the JSON shapes produced
// by internal/admin/admin.go; keep them in sync when the Go handlers change.

// ---- virtual keys ----

// FilterMode constrains a virtual key along one dimension.
export type FilterMode = "none" | "include" | "exclude";

// KeyFilter is a provider or model constraint. "none" allows everything,
// "include" allows only Values, "exclude" allows everything but Values.
export interface KeyFilter {
  mode: FilterMode;
  values: string[];
}

export interface VirtualKey {
  name: string;
  /** Name of the profile whose filters govern this key's model access. */
  profile: string;
  revoked: boolean;
  paused: boolean;
}

export interface KeysResponse {
  keys: VirtualKey[];
}

export interface CreateKeyResponse {
  plaintext: string;
}

/** Fields editable on an existing key (name and its profile). */
export interface UpdateKeyRequest {
  name: string;
  profile: string;
}

// ---- profiles ----

// Profile is a named, reusable provider/model filter shared by virtual keys.
// Its set of allowed models resolves live against the registry, so models
// discovered later are picked up automatically.
export interface Profile {
  name: string;
  providerFilter: KeyFilter;
  modelFilter: KeyFilter;
  /** The seeded "All" profile is read-only. */
  isDefault: boolean;
  /** Number of virtual keys referencing this profile. */
  keyCount: number;
}

export interface ProfilesResponse {
  profiles: Profile[];
}

export interface ProfileRequest {
  name: string;
  providerFilter: KeyFilter;
  modelFilter: KeyFilter;
}

// ---- providers ----

export interface Provider {
  name: string;
  baseURL: string;
  modelCount: number;
  /** Provider disabled by an operator: its models are hidden and unroutable. */
  disabled: boolean;
  /** Result of the last discovery sync; false means the server was unreachable. */
  reachable: boolean;
  /** Error from the last failed sync, empty when reachable. */
  lastError: string;
  /** RFC3339 timestamp of the last successful sync, empty if never. */
  lastSyncedAt: string;
}

export interface CreateProviderRequest {
  name: string;
  baseURL: string;
  apiKey: string;
}

export interface CreateProviderResponse {
  name: string;
  baseURL: string;
  modelCount: number;
  /** Set when the provider was stored but its model catalog could not be fetched. */
  warning?: string;
}

// ---- models ----

// ModelPricing carries per-million-token USD rates. Upstream metadata is
// arbitrary JSON, so unknown pricing keys are allowed.
export interface ModelPricing {
  input?: number;
  output?: number;
  cache_create?: number;
  cache_hit?: number;
  [key: string]: number | string | null | undefined;
}

// ModelMetadata is the merged upstream metadata blob. Only the keys the UI
// reads are named; everything else is kept as unknown.
export interface ModelMetadata {
  pricing?: ModelPricing | null;
  max_input_tokens?: number;
  max_output_tokens?: number;
  context_window?: number;
  // Other names upstreams use for the same limits.
  max_model_len?: number;
  context_length?: number;
  max_context_length?: number;
  max_completion_tokens?: number;
  max_tokens?: number;
  top_provider?: { max_completion_tokens?: number } | null;
  [key: string]: unknown;
}

export interface Model {
  id: number;
  upstream: string;
  upstreamModelId: string;
  gatewayId: string;
  displayName: string;
  /** Custom gateway-ID alias, empty when the model uses its computed ID. */
  alias: string;
  metadata: ModelMetadata;
  disabled: boolean;
  /** Its provider is disabled, so the model is hidden and unroutable. */
  providerDisabled: boolean;
  /** Its provider was unreachable on the last sync. */
  providerReachable: boolean;
}

/** Result of forcing a registry re-discovery from every provider. */
export interface RefreshModelsResponse {
  providers: number;
  models: number;
  warnings: string[];
}

// ---- settings ----

/** Admin-editable runtime settings. */
export interface Settings {
  /** Whether prompt/response bodies are persisted to the content database. */
  storePrompts: boolean;
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

export interface UsageSummary {
  rows: UsageRow[];
  totalReqs: number;
  totalCost: string;
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

export interface RequestDetail {
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

// UsageQuery is the shared from/to/key filter accepted by the usage and
// requests endpoints. Declared as a type alias (not an interface) so it is
// assignable to QueryParams' index signature.
export type UsageQuery = {
  /** Inclusive lower bound as an RFC3339 timestamp. */
  from?: string | null;
  /** Inclusive upper bound as an RFC3339 timestamp. */
  to?: string | null;
  /** Restrict to these virtual key names (repeated as separate params). */
  key?: readonly string[] | null;
};

export type RequestQuery = UsageQuery & {
  limit?: number;
  offset?: number;
};

// ---- errors ----

/** ApiError is the error body returned by the admin API on failure. */
export interface ApiError {
  error: string;
}
