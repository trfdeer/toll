import { create } from "@bufbuild/protobuf";
import {
  ColumnFilterSchema,
  ColumnFilter_ConditionSchema,
  FilterOp,
  ListParamsSchema,
  SortDirection,
  type ColumnFilter,
  type ListParams,
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
  CreateProviderRequest,
  CreateProviderResponse,
  ColumnFilters,
  FilterOp as UiFilterOp,
  ListQuery,
  ModelsResponse,
  ProfileRequest,
  ProfilesResponse,
  ProvidersResponse,
  QueryParams,
  RequestDetail,
  RequestQuery,
  RequestsResponse,
  RefreshModelsResponse,
  UsageListQuery,
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

export const getUsage = (params: UsageListQuery): Promise<UsageSummary> =>
  request(`/usage${qs(listParams(params))}`);

export const getRequests = (params: RequestQuery): Promise<RequestsResponse> =>
  request(`/requests${qs(listParams(params))}`);

export const getRequest = (id: number): Promise<RequestDetail> =>
  request(`/requests/${encodeURIComponent(id)}`);

export const getProviders = (
  params?: ListQuery,
): Promise<ProvidersResponse> =>
  request(`/providers${qs(listParams(params))}`);

export const addProvider = (
  provider: CreateProviderRequest,
): Promise<CreateProviderResponse> =>
  request("/providers", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(provider),
  });

export const deleteProvider = (name: string): Promise<void> =>
  request(`/providers/${encodeURIComponent(name)}`, { method: "DELETE" });

export const disableProvider = (name: string): Promise<void> =>
  request(`/providers/${encodeURIComponent(name)}/disable`, { method: "POST" });

export const enableProvider = (name: string): Promise<void> =>
  request(`/providers/${encodeURIComponent(name)}/enable`, { method: "POST" });

export const getModels = (params?: ListQuery): Promise<ModelsResponse> =>
  request(`/models${qs(listParams(params))}`);

// refreshModels forces every provider's catalog to be re-pulled server-side.
export const refreshModels = (): Promise<RefreshModelsResponse> =>
  request("/models/refresh", { method: "POST" });

export const deleteModel = (id: number): Promise<void> =>
  request(`/models/${encodeURIComponent(id)}`, { method: "DELETE" });

export const disableModel = (id: number): Promise<void> =>
  request(`/models/${encodeURIComponent(id)}/disable`, { method: "POST" });

export const enableModel = (id: number): Promise<void> =>
  request(`/models/${encodeURIComponent(id)}/enable`, { method: "POST" });

// setModelAlias sets or clears a model's custom gateway ID; an empty alias
// restores the computed one.
export const setModelAlias = (id: number, alias: string): Promise<void> =>
  request(`/models/${encodeURIComponent(id)}/alias`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ alias }),
  });

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
// the default set-membership op.
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
};

// filtersProto encodes the UI's per-column filters as proto ColumnFilters.
// Today's UI sends one condition per column; the schema's multi-condition
// filters arrive with the migration's filter-engine phase. An empty "in"
// set means match-nothing, exactly like the server's semantics.
function filtersProto(
  filters: ColumnFilters | undefined,
): { [key: string]: ColumnFilter } {
  const out: { [key: string]: ColumnFilter } = {};
  for (const [col, spec] of Object.entries(filters ?? {})) {
    out[col] = create(ColumnFilterSchema, {
      conditions: [
        create(ColumnFilter_ConditionSchema, {
          op: PROTO_OPS[spec.op] ?? FilterOp.IN,
          values: spec.values,
        }),
      ],
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
    for (const v of status.values) {
      if (v === "revoked") {
        cols.revoked = { op: "in", values: ["true"] };
      } else if (v === "paused") {
        cols.paused = { op: "in", values: ["true"] };
        cols.revoked = { op: "in", values: ["false"] };
      } else if (v === "active") {
        cols.paused = { op: "in", values: ["false"] };
        cols.revoked = { op: "in", values: ["false"] };
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
