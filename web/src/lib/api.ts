import { create } from "@bufbuild/protobuf";
import {
  timestampDate,
  timestampFromDate,
} from "@bufbuild/protobuf/wkt";
import {
  ColumnFilterSchema,
  ColumnFilter_ConditionSchema,
  FilterJoin,
  FilterOp,
  ListParamsSchema,
  SortDirection,
  UsageFilterSchema,
  type ColumnFilter,
  type ListParams,
  type UsageFilter,
} from "../gen/toll/admin/v1/common_pb";
import {
  DeleteKeyRequestSchema,
  ListKeysRequestSchema,
  RevokeKeyRequestSchema,
  RotateKeyRequestSchema,
  UpdateKeyRequestSchema,
  type VirtualKey,
} from "../gen/toll/admin/v1/keys_pb";
import {
  UpdateSettingsRequestSchema,
  type Settings,
} from "../gen/toll/admin/v1/settings_pb";
import type {
  ColumnFilters,
  FilterOp as UiFilterOp,
  ListQuery,
  ProfileRequest,
  ProfilesResponse,
  QueryParams,
  RequestsResponse,
  RequestDetailView,
  UsageSummary,
} from "./types";
import { admin } from "./connect";

// request performs a JSON call against the admin API. Bodies come from the
// same-origin Go server, so the generic cast is applied at this single
// boundary rather than sprinkled through the views. Endpoints that return 204
// have no body and are typed as void by their callers.
async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const res = await fetch(`/admin/api${path}`, options);
  if (!res.ok) {
    throw new Error(await errorFromResponse(res));
  }
  if (res.status === 204) {
    return undefined as T;
  }
  return (await res.json()) as T;
}

// errorFromResponse extracts the API's {"error": "..."} message, falling back
// to the HTTP status text when the body is missing or not JSON.
async function errorFromResponse(res: Response): Promise<string> {
  let msg = res.statusText;
  try {
    const body: unknown = await res.json();
    if (
      body !== null &&
      typeof body === "object" &&
      "error" in body &&
      typeof body.error === "string"
    ) {
      msg = body.error;
    }
  } catch {
    // not JSON — keep statusText
  }
  return msg;
}

// qs builds a query string from params, dropping empty values. Array values
// (e.g. multiple key filters) are repeated as separate params.
function qs(params: QueryParams | undefined): string {
  const search = new URLSearchParams();
  for (const [k, v] of Object.entries(params ?? {})) {
    if (v === undefined || v === null || v === "") continue;
    if (typeof v === "object") {
      for (const item of v) {
        if (item !== "") search.append(k, item);
      }
    } else {
      search.set(k, String(v));
    }
  }
  const s = search.toString();
  return s ? `?${s}` : "";
}

// listParams flattens a ListQuery into query params, encoding the column
// filters as the API's single JSON `filter` parameter.
function listParams(q: ListQuery | undefined): QueryParams {
  if (!q) return {};
  const { filters, ...rest } = q;
  return {
    ...rest,
    filter:
      filters && Object.keys(filters).length > 0
        ? JSON.stringify(filters)
        : undefined,
  };
}

// ---- usage & requests (ConnectRPC) ----

// usageQuery is the shared from/to/key narrowing. The UI's "(deleted keys)"
// pseudo-entry maps onto includeDeletedKeys; real key names ride keys.
export interface UsageQuery {
  from?: string | null;
  to?: string | null;
  keys?: readonly string[];
  includeDeletedKeys?: boolean;
}

// usageFilterProto encodes the shared narrowing. Bounds are inclusive and
// ride as proto Timestamps; absent means unbounded.
function usageFilterProto(q: UsageQuery): UsageFilter {
  return create(UsageFilterSchema, {
    keys: q.keys ? [...q.keys] : [],
    includeDeletedKeys: q.includeDeletedKeys ?? false,
    from: q.from ? timestampFromDate(new Date(q.from)) : undefined,
    to: q.to ? timestampFromDate(new Date(q.to)) : undefined,
  });
}

export const getUsage = async (
  params: UsageQuery & ListQuery,
): Promise<UsageSummary> => {
  const res = await admin.getUsage({
    filter: usageFilterProto(params),
    params: listParamsProto(params),
  });
  return {
    // int64 token sums arrive as bigint; the view layer works in numbers
    // (exact to 2^53, far past any token count).
    rows: res.rows.map((r) => ({
      gatewayModel: r.gatewayModel,
      requests: Number(r.requests),
      promptTokens: Number(r.promptTokens),
      cachedTokens: Number(r.cachedTokens),
      completionTokens: Number(r.completionTokens),
      costUSD: r.costUsd,
    })),
    total: Number(res.total),
    totals: {
      requests: Number(res.totals?.requests ?? 0n),
      promptTokens: Number(res.totals?.promptTokens ?? 0n),
      cachedTokens: Number(res.totals?.cachedTokens ?? 0n),
      completionTokens: Number(res.totals?.completionTokens ?? 0n),
      costUSD: res.totals?.costUsd ?? 0,
    },
  };
};

export const getRequests = async (
  params: UsageQuery & ListQuery,
): Promise<RequestsResponse> => {
  const res = await admin.listRequests({
    filter: usageFilterProto(params),
    params: listParamsProto(params),
  });
  return {
    requests: res.requests.map((t) => ({
      id: Number(t.id),
      conversationId: t.conversationId,
      // A deleted key renders an empty name; the UI shows the placeholder.
      keyName: t.keyName,
      gatewayModel: t.gatewayModel,
      status: t.status,
      promptTokens: Number(t.promptTokens),
      cachedTokens: Number(t.cachedTokens),
      completionTokens: Number(t.completionTokens),
      costUSD: t.costUsd === undefined ? null : t.costUsd,
      createdAt: t.createdAt ? timestampDate(t.createdAt).toISOString() : "",
      durationMs: t.duration
        ? Number(t.duration.seconds) * 1000 + t.duration.nanos / 1e6
        : null,
    })),
    total: Number(res.total),
  };
};

// getRequest returns one transcript normalized into a conversation.
export const getRequest = async (id: number): Promise<RequestDetailView> => {
  const res = await admin.getRequest({ id: BigInt(id) });
  return {
    id: Number(res.id),
    conversationId: res.conversationId,
    gatewayModel: res.gatewayModel,
    upstreamModel: res.upstreamModel,
    status: res.status,
    createdAt: res.createdAt ? timestampDate(res.createdAt).toISOString() : "",
    completedAt: res.completedAt
      ? timestampDate(res.completedAt).toISOString()
      : "",
    durationMs: res.duration
      ? Number(res.duration.seconds) * 1000 + res.duration.nanos / 1e6
      : null,
    contentStored: res.contentStored,
    messages: res.messages.map((m) => ({
      role: m.role,
      content: m.content,
      reasoning: m.reasoning || undefined,
      name: m.name || undefined,
      toolCallId: m.toolCallId || undefined,
      toolCalls: m.toolCalls.map((tc) => ({
        id: tc.id || undefined,
        name: tc.name,
        arguments: tc.arguments || undefined,
      })),
      finishReason: m.finishReason || undefined,
    })),
  };
};

// ---- providers (ConnectRPC) ----

// The providers table's derived "status" column translates into the schema's
// disabled/reachable boolean columns (an AND across the two, per value).
function providersParams(q: ListQuery | undefined): ListParams {
  const { filters, ...rest } = q ?? {};
  const { status, ...cols } = filters ?? {};
  if (status) {
    // The set filter arrives as one "in" condition; its values drive the
    // disabled/reachable boolean columns.
    for (const v of status.conditions.flatMap((c) => c.values)) {
      if (v === "disabled") {
        cols.disabled = { conditions: [{ op: "in", values: ["true"] }] };
      } else if (v === "active") {
        cols.disabled = { conditions: [{ op: "in", values: ["false"] }] };
        cols.reachable = { conditions: [{ op: "in", values: ["true"] }] };
      } else if (v === "unreachable") {
        cols.disabled = { conditions: [{ op: "in", values: ["false"] }] };
        cols.reachable = { conditions: [{ op: "in", values: ["false"] }] };
      }
    }
  }
  return listParamsProto({ ...rest, filters: cols });
}

export const getProviders = async (params?: ListQuery) => {
  const res = await admin.listProviders({ params: providersParams(params) });
  return { providers: res.providers, total: res.total };
};

// addProvider registers an upstream and returns it after the best-effort
// initial catalog sync, with a warning when that fetch failed.
export const addProvider = (provider: {
  name: string;
  baseURL: string;
  apiKey: string;
}) => admin.createProvider({ ...provider });

// updateProvider toggles a provider; base URL and key stay immutable.
export const updateProvider = (name: string, disabled: boolean) =>
  admin.updateProvider({ name, disabled });

export const deleteProvider = (name: string) =>
  admin.deleteProvider({ name });

// ---- models (ConnectRPC) ----

export const getModels = async (params?: ListQuery) => {
  const res = await admin.listModels({ params: listParamsProto(params) });
  return { models: res.models, total: res.total };
};

// refreshModels forces every provider's catalog to be re-pulled server-side;
// per-provider failures come back as structured warnings.
export const refreshModels = async () => {
  const res = await admin.refreshModels({});
  return {
    providers: res.providers,
    models: res.models,
    warnings: res.warnings.map((w) => `${w.name}: ${w.error}`),
  };
};

// deleteModel removes one registry entry; ids are the schema's int64 (bigint).
export const deleteModel = (id: bigint) => admin.deleteModel({ id });

// updateModel toggles a model and/or sets its custom gateway ID (an empty
// alias restores the computed one); absent fields are left unchanged.
export const updateModel = (
  id: bigint,
  update: { disabled?: boolean; alias?: string },
) => admin.updateModel({ id, ...update });

// ---- settings (ConnectRPC) ----

export const getSettings = (): Promise<Settings> => admin.getSettings({});

// updateSettings persists the runtime settings and returns the saved state.
export const updateSettings = (settings: { storePrompts: boolean }): Promise<Settings> =>
  admin.updateSettings(create(UpdateSettingsRequestSchema, settings));

// exportConfig fetches the gateway config (toll.yaml) for the current
// registry state; secrets are redacted to api_key_env references.
export const exportConfig = async (): Promise<string> => {
  const res = await admin.exportConfig({});
  return res.yaml;
};

// ---- profiles ----

export const getProfiles = (params?: ListQuery): Promise<ProfilesResponse> =>
  request(`/profiles${qs(listParams(params))}`);

export const createProfile = (profile: ProfileRequest): Promise<void> =>
  request("/profiles", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(profile),
  });

export const updateProfile = (
  currentName: string,
  profile: ProfileRequest,
): Promise<void> =>
  request(`/profiles/${encodeURIComponent(currentName)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(profile),
  });

export const deleteProfile = (name: string): Promise<void> =>
  request(`/profiles/${encodeURIComponent(name)}`, { method: "DELETE" });

// ---- ConnectRPC list params ----

// PROTO_OPS maps the UI's filter operator ids onto the proto enum. "in" is
// the default set-membership op; before/after already mapped onto gt/lt in
// the UI layer.
const PROTO_OPS: Record<UiFilterOp, FilterOp> = {
  in: FilterOp.IN,
  contains: FilterOp.CONTAINS,
  notContains: FilterOp.NOT_CONTAINS,
  equals: FilterOp.EQUALS,
  notEquals: FilterOp.NOT_EQUALS,
  startsWith: FilterOp.STARTS_WITH,
  endsWith: FilterOp.ENDS_WITH,
  blank: FilterOp.BLANK,
  notBlank: FilterOp.NOT_BLANK,
  gt: FilterOp.GT,
  gte: FilterOp.GTE,
  lt: FilterOp.LT,
  lte: FilterOp.LTE,
  between: FilterOp.BETWEEN,
};

// filtersProto encodes the UI's per-column filters as proto ColumnFilters,
// conditions and join intact. An empty "in" set means match-nothing, exactly
// like the server's semantics.
function filtersProto(
  filters: ColumnFilters | undefined,
): { [key: string]: ColumnFilter } {
  const out: { [key: string]: ColumnFilter } = {};
  for (const [col, spec] of Object.entries(filters ?? {})) {
    out[col] = create(ColumnFilterSchema, {
      conditions: spec.conditions.map((c) =>
        create(ColumnFilter_ConditionSchema, {
          op: PROTO_OPS[c.op] ?? FilterOp.IN,
          values: c.values,
        }),
      ),
      join:
        spec.join === "and"
          ? FilterJoin.AND
          : spec.join === "or"
            ? FilterJoin.OR
            : FilterJoin.UNSPECIFIED,
    });
  }
  return out;
}

// listParamsProto encodes a ListQuery as the proto ListParams. limit 0 keeps
// its "every row" meaning; an absent limit selects the endpoint's default.
export function listParamsProto(q: ListQuery | undefined): ListParams {
  return create(ListParamsSchema, {
    limit: q?.limit,
    offset: q?.offset ?? 0,
    sort: q?.sort ?? "",
    dir:
      q?.dir === "asc"
        ? SortDirection.ASC
        : q?.dir === "desc"
          ? SortDirection.DESC
          : SortDirection.UNSPECIFIED,
    filter: filtersProto(q?.filters),
  });
}

// ---- virtual keys (ConnectRPC pilot) ----

// KeysResponse is the keys list result, with the generated VirtualKey rows.
export interface KeysResponse {
  keys: VirtualKey[];
  total: number;
}

// The keys table's column ids per the schema: name, profile, revoked, paused.
// The legacy "status" set filter is translated into those boolean columns.
function keysParams(q: ListQuery | undefined): ListParams {
  const { filters, ...rest } = q ?? {};
  const { status, ...cols } = filters ?? {};
  if (status) {
    // The set filter arrives as one "in" condition; its values drive the
    // revoked/paused boolean columns.
    for (const v of status.conditions.flatMap((c) => c.values)) {
      if (v === "revoked") {
        cols.revoked = { conditions: [{ op: "in", values: ["true"] }] };
      } else if (v === "paused") {
        cols.paused = { conditions: [{ op: "in", values: ["true"] }] };
        cols.revoked = { conditions: [{ op: "in", values: ["false"] }] };
      } else if (v === "active") {
        cols.paused = { conditions: [{ op: "in", values: ["false"] }] };
        cols.revoked = { conditions: [{ op: "in", values: ["false"] }] };
      }
    }
  }
  return listParamsProto({ ...rest, filters: cols });
}

export const getKeys = async (params?: ListQuery): Promise<KeysResponse> => {
  const res = await admin.listKeys(
    create(ListKeysRequestSchema, { params: keysParams(params) }),
  );
  return { keys: res.keys, total: res.total };
};

// createKey returns the created key next to the one-time plaintext.
export const createKey = (name: string, profile: string) =>
  admin.createKey({ name, profile: profile === "All" ? undefined : profile });

// updateKey edits a key in place; only the fields the caller sets change.
export const updateKey = (
  currentName: string,
  update: { newName?: string; profile?: string; paused?: boolean },
) =>
  admin.updateKey(
    create(UpdateKeyRequestSchema, { name: currentName, ...update }),
  );

// rotateKey replaces a key's secret and returns the new plaintext once.
export const rotateKey = (name: string) =>
  admin.rotateKey(create(RotateKeyRequestSchema, { name }));

export const revokeKey = (name: string) =>
  admin.revokeKey(create(RevokeKeyRequestSchema, { name }));

export const deleteKey = (name: string) =>
  admin.deleteKey(create(DeleteKeyRequestSchema, { name }));
