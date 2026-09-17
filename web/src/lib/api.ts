import type {
  CreateKeyResponse,
  CreateProviderRequest,
  CreateProviderResponse,
  KeyFilter,
  KeysResponse,
  Model,
  Provider,
  QueryParams,
  RequestDetail,
  RequestQuery,
  RequestsResponse,
  RefreshModelsResponse,
  Settings,
  UpdateKeyRequest,
  UsageQuery,
  UsageSummary,
} from "./types";

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

export const getUsage = (params: UsageQuery): Promise<UsageSummary> =>
  request(`/usage${qs(params)}`);

export const getRequests = (params: RequestQuery): Promise<RequestsResponse> =>
  request(`/requests${qs(params)}`);

export const getRequest = (id: number): Promise<RequestDetail> =>
  request(`/requests/${encodeURIComponent(id)}`);

export const getProviders = (): Promise<Provider[]> => request("/providers");

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

export const getModels = (): Promise<Model[]> => request("/models");

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

// ---- settings ----

export const getSettings = (): Promise<Settings> => request("/settings");

// updateSettings persists the runtime settings and returns the saved state.
export const updateSettings = (settings: Settings): Promise<Settings> =>
  request("/settings", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(settings),
  });

export const getKeys = (): Promise<KeysResponse> => request("/keys");

export const createKey = (
  name: string,
  providerFilter: KeyFilter,
  modelFilter: KeyFilter,
): Promise<CreateKeyResponse> =>
  request("/keys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, providerFilter, modelFilter }),
  });

// updateKey edits an existing key in place (its secret is unchanged).
export const updateKey = (
  currentName: string,
  update: UpdateKeyRequest,
): Promise<void> =>
  request(`/keys/${encodeURIComponent(currentName)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(update),
  });

export const revokeKey = (name: string): Promise<void> =>
  request(`/keys/${encodeURIComponent(name)}/revoke`, { method: "POST" });

export const pauseKey = (name: string): Promise<void> =>
  request(`/keys/${encodeURIComponent(name)}/pause`, { method: "POST" });

export const resumeKey = (name: string): Promise<void> =>
  request(`/keys/${encodeURIComponent(name)}/resume`, { method: "POST" });

export const deleteKey = (name: string): Promise<void> =>
  request(`/keys/${encodeURIComponent(name)}`, { method: "DELETE" });

// exportConfig fetches the gateway config (toll.yaml) for the current
// registry state. The endpoint returns YAML, not JSON, so this bypasses the
// shared request helper.
export async function exportConfig(): Promise<string> {
  const res = await fetch("/admin/api/config");
  if (!res.ok) {
    throw new Error(await errorFromResponse(res));
  }
  return res.text();
}
