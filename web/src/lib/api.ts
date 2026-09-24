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
  ColumnFilters,
  FilterOp as UiFilterOp,
  ListQuery,
  ProfileRequest,
  ProfilesResponse,
  QueryParams,
  RequestDetail,
  RequestQuery,
  RequestsResponse,
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

// ---- providers (ConnectRPC) ----

// The providers table's derived "status" column translates into the schema's
// disabled/reachable boolean columns (an AND across the two, per value).
function providersParams(q: ListQuery | undefined): ListParams {
  const { filters, ...rest } = q ?? {};
  const { status, ...cols } = filters ?? {};
  if (status) {
    for (const v of status.values) {
      if (v === "disabled") {
        cols.disabled = { op: "in", values: ["true"] };
      } else if (v === "active") {
        cols.disabled = { op: "in", values: ["false"] };
        cols.reachable = { op: "in", values: ["true"] };
      } else if (v === "unreachable") {
        cols.disabled = { op: "in", values: ["false"] };
        cols.reachable = { op: "in", values: ["false"] };
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
