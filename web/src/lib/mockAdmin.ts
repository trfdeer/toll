// In-memory implementation of the migrated toll.admin.v1 AdminService RPCs
// for the Vite dev server. The routes are registered per method on the dev
// router, so an unimplemented RPC fails exactly like the real server's
// strangled surface does, and each implementation is type-checked against the
// generated method descriptor — the mock cannot drift from the schema.
import { create } from "@bufbuild/protobuf";
import {
  durationFromMs,
  EmptySchema,
  timestampDate,
  timestampFromDate,
} from "@bufbuild/protobuf/wkt";
import type { JsonObject } from "@bufbuild/protobuf";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";
import { AdminService } from "../gen/toll/admin/v1/admin_pb";
import {
  FilterOp,
  KeyFilter_Mode,
  KeyFilterSchema,
  SortDirection,
  type ColumnFilter,
  type KeyFilter as KeyFilterProto,
  type ListParams,
  type UsageFilter,
} from "../gen/toll/admin/v1/common_pb";
import {
  GetUsageResponseSchema,
  UsageRowSchema,
  type GetUsageRequest,
} from "../gen/toll/admin/v1/usage_pb";
import {
  GetUsageSeriesResponseSchema,
  ListFilterValuesResponseSchema,
  UsageSeriesPointSchema,
  SeriesGrouping,
  type GetUsageSeriesRequest,
  type ListFilterValuesRequest,
  type UsageSeriesPoint,
} from "../gen/toll/admin/v1/stats_pb";
import {
  ListRequestsResponseSchema,
  RequestDetailSchema,
  type GetRequestRequest,
  type ListRequestsRequest,
  type RequestDetail,
} from "../gen/toll/admin/v1/requests_pb";
import {
  ListProfilesResponseSchema,
  ProfileSchema,
  type CreateProfileRequest,
  type DeleteProfileRequest,
  type ListProfilesRequest,
  type Profile,
  type UpdateProfileRequest,
} from "../gen/toll/admin/v1/profiles_pb";
import {
  CreateKeyResponseSchema,
  ListKeysResponseSchema,
  RotateKeyResponseSchema,
  VirtualKeySchema,
  type CreateKeyRequest,
  type DeleteKeyRequest,
  type ListKeysRequest,
  type RevokeKeyRequest,
  type RotateKeyRequest,
  type UpdateKeyRequest,
  type VirtualKey,
} from "../gen/toll/admin/v1/keys_pb";
import {
  ExportConfigResponseSchema,
  SettingsSchema,
  type UpdateSettingsRequest,
} from "../gen/toll/admin/v1/settings_pb";
import {
  CreateProviderResponseSchema,
  ListProvidersResponseSchema,
  ProviderSchema,
  type CreateProviderRequest,
  type DeleteProviderRequest,
  type ListProvidersRequest,
  type Provider,
  type UpdateProviderRequest,
} from "../gen/toll/admin/v1/providers_pb";
import {
  ListModelsResponseSchema,
  ModelSchema,
  RefreshModelsResponseSchema,
  type DeleteModelRequest,
  type ListModelsRequest,
  type Model,
  type UpdateModelRequest,
} from "../gen/toll/admin/v1/models_pb";

// The same seed data the dev SPA showed before the REST mock was removed.
const keys: VirtualKey[] = [
  create(VirtualKeySchema, { name: "web", profile: "All" }),
  create(VirtualKeySchema, { name: "batch", profile: "hyper-chat", revoked: true }),
];

const notFound = (name: string) =>
  new ConnectError(`virtual key ${name} not found`, Code.NotFound);

const find = (name: string): VirtualKey => {
  const k = keys.find((k) => k.name === name);
  if (!k) throw notFound(name);
  return k;
};

// cell renders one column's value as the text the filter ops match against,
// mirroring the server's boolean columns ('true'/'false').
function cell(k: VirtualKey, col: string): string {
  switch (col) {
    case "name":
      return k.name;
    case "profile":
      return k.profile;
    case "revoked":
      return k.revoked ? "true" : "false";
    case "paused":
      return k.paused ? "true" : "false";
    default:
      throw new ConnectError(`unknown filter column "${col}"`, Code.InvalidArgument);
  }
}

// matchesCell applies one column filter to a rendered cell, mirroring the
// server's text-op semantics (case-insensitive, "" matches blank cells).
function matchesCell(s: string, f: ColumnFilter): boolean {
  if (f.conditions.length === 0) {
    throw new ConnectError("at least one condition is required", Code.InvalidArgument);
  }
  const lv = s.toLowerCase();
  return f.conditions.some((c) => {
    const vals = c.values;
    switch (c.op) {
      case FilterOp.UNSPECIFIED:
      case FilterOp.IN:
        if (vals.includes("") && s === "") return true;
        return vals.includes(s);
      case FilterOp.CONTAINS:
        return vals.some((v) => lv.includes(v.toLowerCase()));
      case FilterOp.NOT_CONTAINS:
        return !vals.some((v) => lv.includes(v.toLowerCase()));
      case FilterOp.EQUALS:
        return vals.some((v) => lv === v.toLowerCase());
      case FilterOp.NOT_EQUALS:
        return !vals.some((v) => lv === v.toLowerCase());
      case FilterOp.STARTS_WITH:
        return vals.some((v) => lv.startsWith(v.toLowerCase()));
      case FilterOp.ENDS_WITH:
        return vals.some((v) => lv.endsWith(v.toLowerCase()));
      case FilterOp.BLANK:
        return s === "";
      case FilterOp.NOT_BLANK:
        return s !== "";
      case FilterOp.GT:
      case FilterOp.GTE:
      case FilterOp.LT:
      case FilterOp.LTE: {
        const bound = Number(vals[0]);
        if (Number.isNaN(bound)) {
          throw new ConnectError("op requires a numeric value", Code.InvalidArgument);
        }
        const cell = Number(s);
        // Non-numeric cells never match the numeric ops.
        if (Number.isNaN(cell)) return false;
        switch (c.op) {
          case FilterOp.GT:
            return cell > bound;
          case FilterOp.GTE:
            return cell >= bound;
          case FilterOp.LT:
            return cell < bound;
          default:
            return cell <= bound;
        }
      }
      case FilterOp.BETWEEN: {
        const lower = vals[0] ? Number(vals[0]) : null;
        const upper = vals[1] ? Number(vals[1]) : null;
        if (lower === null && upper === null) {
          throw new ConnectError("op between requires a bound", Code.InvalidArgument);
        }
        const cell = Number(s);
        if (Number.isNaN(cell)) return false;
        return (lower === null || cell >= lower) && (upper === null || cell <= upper);
      }
      default:
        throw new ConnectError("op is not supported yet", Code.InvalidArgument);
    }
  });
}

// matches applies one column filter to a keys row.
function matches(k: VirtualKey, col: string, f: ColumnFilter): boolean {
  return matchesCell(cell(k, col), f);
}

// listWindow sorts (with a tiebreak on the source order), then applies the
// page window: absent limit = 50, 0 = all rows.
function listWindow<T>(
  rows: T[],
  p: ListParams | undefined,
  sort: string,
  accessors: Record<string, (row: T) => string | number>,
  defaultSort: string,
): T[] {
  const dir = p?.dir === SortDirection.DESC ? -1 : 1;
  const acc = accessors[p?.sort || sort || defaultSort];
  if (!acc) {
    throw new ConnectError(`unknown sort column "${sort}"`, Code.InvalidArgument);
  }
  return rows
    .map((row, i) => ({ row, i }))
    .sort((a, b) => {
      const av = acc(a.row);
      const bv = acc(b.row);
      const cmp =
        typeof av === "number" && typeof bv === "number"
          ? av - bv
          : String(av).localeCompare(String(bv), undefined, { numeric: true });
      return cmp * dir || (a.i - b.i) * dir;
    })
    .map((x) => x.row);
}

function page<T>(rows: T[], p: ListParams | undefined): T[] {
  const limit = p?.limit === undefined ? 50 : p.limit;
  const offset = p?.offset ?? 0;
  return limit > 0 ? rows.slice(offset, offset + limit) : rows.slice(offset);
}

// listKeys mirrors the Go list endpoint: ANDed per-column filters, then
// sort (with the id tiebreak), then paging (absent limit = 50, 0 = all).
function listKeys(req: ListKeysRequest) {
  const p = req.params;
  let rows = keys.slice();
  for (const [col, f] of Object.entries(p?.filter ?? {})) {
    rows = rows.filter((k) => matches(k, col, f));
  }
  rows = listWindow(rows, p, p?.sort ?? "", {
    id: (k) => keys.indexOf(k),
    name: (k) => k.name,
    profile: (k) => k.profile,
    revoked: (k) => (k.revoked ? 1 : 0),
    paused: (k) => (k.paused ? 1 : 0),
  }, "name");

  const total = rows.length;
  return create(ListKeysResponseSchema, { keys: page(rows, p), total });
}

function createKey(req: CreateKeyRequest) {
  const name = req.name.trim();
  if (name === "") {
    throw new ConnectError("name is required", Code.InvalidArgument);
  }
  if (keys.some((k) => k.name === name)) {
    throw new ConnectError(`virtual key name already exists`, Code.AlreadyExists);
  }
  const key = create(VirtualKeySchema, {
    name,
    profile: req.profile || "All",
  });
  keys.push(key);
  return create(CreateKeyResponseSchema, {
    key,
    plaintext: "sk-tl-mock-" + Math.random().toString(36).slice(2, 12),
  });
}

function updateKey(req: UpdateKeyRequest) {
  const k = find(req.name);
  if (req.newName !== undefined) {
    const name = req.newName.trim();
    if (name === "") {
      throw new ConnectError("new_name is required", Code.InvalidArgument);
    }
    if (name !== req.name && keys.some((x) => x.name === name)) {
      throw new ConnectError("virtual key name already exists", Code.AlreadyExists);
    }
    k.name = name;
  }
  if (req.profile !== undefined) {
    k.profile = req.profile || "All";
  }
  if (req.paused !== undefined && !k.revoked) {
    k.paused = req.paused;
  }
  return create(VirtualKeySchema, { ...k });
}

function rotateKey(req: RotateKeyRequest) {
  const k = find(req.name);
  return create(RotateKeyResponseSchema, {
    key: k,
    plaintext: "sk-tl-mock-" + Math.random().toString(36).slice(2, 12),
  });
}

function revokeKey(req: RevokeKeyRequest) {
  find(req.name).revoked = true;
  return create(EmptySchema);
}

function deleteKey(req: DeleteKeyRequest) {
  const i = keys.findIndex((k) => k.name === req.name);
  if (i < 0) throw notFound(req.name);
  keys.splice(i, 1);
  return create(EmptySchema);
}

// mockAdminRoutes registers the migrated RPCs on a dev ConnectRouter.
export function mockAdminRoutes(router: ConnectRouter) {
  router.rpc(AdminService.method.listKeys, listKeys);
  router.rpc(AdminService.method.createKey, createKey);
  router.rpc(AdminService.method.updateKey, updateKey);
  router.rpc(AdminService.method.rotateKey, rotateKey);
  router.rpc(AdminService.method.revokeKey, revokeKey);
  router.rpc(AdminService.method.deleteKey, deleteKey);
  router.rpc(AdminService.method.getSettings, getSettings);
  router.rpc(AdminService.method.updateSettings, updateSettings);
  router.rpc(AdminService.method.exportConfig, exportConfig);
  router.rpc(AdminService.method.listProviders, listProviders);
  router.rpc(AdminService.method.createProvider, createProvider);
  router.rpc(AdminService.method.updateProvider, updateProvider);
  router.rpc(AdminService.method.deleteProvider, deleteProvider);
  router.rpc(AdminService.method.listModels, listModels);
  router.rpc(AdminService.method.refreshModels, refreshModels);
  router.rpc(AdminService.method.updateModel, updateModel);
  router.rpc(AdminService.method.deleteModel, deleteModel);
  router.rpc(AdminService.method.getUsage, getUsage);
  router.rpc(AdminService.method.listRequests, listRequests);
  router.rpc(AdminService.method.getRequest, getRequest);
  router.rpc(AdminService.method.listProfiles, listProfiles);
  router.rpc(AdminService.method.createProfile, createProfileMock);
  router.rpc(AdminService.method.updateProfile, updateProfileMock);
  router.rpc(AdminService.method.deleteProfile, deleteProfileMock);
  router.rpc(AdminService.method.getUsageSeries, getUsageSeriesMock);
  router.rpc(AdminService.method.listFilterValues, listFilterValuesMock);
}

// ---- providers & models ----

// The same seed data the dev REST mock showed before it was removed.
const providers: Provider[] = [
  create(ProviderSchema, {
    name: "hyper",
    baseUrl: "http://zeph:9931/v1",
    modelCount: 3,
    disabled: false,
    reachable: true,
    lastError: "",
    lastSyncedAt: timestampFromDate(new Date("2026-09-15T00:00:00Z")),
  }),
];
const models: Model[] = [
  modelRow(BigInt(1), "hyper", "glm-4.6", "GLM 4.6", "", { max_model_len: 262144, max_output_tokens: 8192 }, false),
  modelRow(BigInt(2), "hyper", "glm-4.5-air", "GLM 4.5 Air", "", { context_window: 128000 }, false),
  modelRow(BigInt(3), "hyper", "deepseek-v3", "DeepSeek V3", "", {}, true),
];

// epoch is the zero timestamp for providers that never synced.
const epoch = timestampFromDate(new Date(0));

// modelRow builds one registry entry; gateway IDs map one-to-one in the mock.
function modelRow(
  id: bigint,
  upstream: string,
  upstreamModelId: string,
  displayName: string,
  alias: string,
  metadata: JsonObject,
  disabled: boolean,
): Model {
  return create(ModelSchema, {
    id,
    upstream,
    upstreamModelId,
    gatewayId: alias || `${upstream}/${upstreamModelId}`,
    displayName,
    alias,
    metadata,
    disabled,
    providerDisabled: false,
    providerReachable: true,
  });
}

// providerCells renders a provider column for the filter ops.
const providerCells: Record<string, (p: Provider) => string> = {
  name: (p) => p.name,
  baseURL: (p) => p.baseUrl,
  disabled: (p) => (p.disabled ? "true" : "false"),
  reachable: (p) => (p.reachable ? "true" : "false"),
};

function listProviders(req: ListProvidersRequest) {
  const p = req.params;
  let rows = providers.slice();
  for (const [col, f] of Object.entries(p?.filter ?? {})) {
    const get = providerCells[col];
    if (!get) {
      throw new ConnectError(`unknown filter column "${col}"`, Code.InvalidArgument);
    }
    rows = rows.filter((x) => matchesCell(get(x), f));
  }
  rows = listWindow(rows, p, p?.sort ?? "", {
    id: (_p) => 0,
    position: (_p) => 0,
    name: (x) => x.name,
    baseURL: (x) => x.baseUrl,
    modelCount: (x) => x.modelCount,
    disabled: (x) => (x.disabled ? 1 : 0),
    reachable: (x) => (x.reachable ? 1 : 0),
    lastSyncedAt: (x) => timestampDate(x.lastSyncedAt ?? epoch).getTime(),
  }, "position");

  const total = rows.length;
  return create(ListProvidersResponseSchema, { providers: page(rows, p), total });
}

function createProvider(req: CreateProviderRequest) {
  const name = req.name.trim();
  const baseURL = req.baseUrl.trim();
  if (name === "" || baseURL === "" || req.apiKey === "") {
    throw new ConnectError("name, baseURL and apiKey are required", Code.InvalidArgument);
  }
  const provider = create(ProviderSchema, {
    name,
    baseUrl: baseURL,
    modelCount: 0,
    disabled: false,
    reachable: false,
    lastError: "dev mock: model sync is not simulated",
  });
  providers.push(provider);
  return create(CreateProviderResponseSchema, {
    provider,
    warning: "provider added, but its models could not be fetched (dev mock)",
  });
}

function updateProvider(req: UpdateProviderRequest) {
  const p = providers.find((x) => x.name === req.name);
  if (!p) throw new ConnectError(`provider ${req.name} not found`, Code.NotFound);
  if (req.disabled !== undefined) p.disabled = req.disabled;
  return create(ProviderSchema, { ...p });
}

function deleteProvider(req: DeleteProviderRequest) {
  const i = providers.findIndex((x) => x.name === req.name);
  if (i < 0) throw new ConnectError(`provider ${req.name} not found`, Code.NotFound);
  providers.splice(i, 1);
  for (let j = models.length - 1; j >= 0; j--) {
    if (models[j]?.upstream === req.name) models.splice(j, 1);
  }
  return create(EmptySchema);
}

// Metadata-derived model columns, mirroring the model_search view's
// json_extract coalesce chains.
const modelMeta = (m: Model, path: string): unknown => {
  let cur: unknown = m.metadata;
  for (const part of path.split(".")) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[part];
  }
  return cur;
};
const metaLimit = (m: Model, paths: string[]): number | string => {
  for (const path of paths) {
    const v = modelMeta(m, path);
    if (typeof v === "number") return v;
    if (typeof v === "string" && v.trim() !== "") return v;
  }
  return "";
};
const INPUT_LIMITS = ["max_input_tokens", "context_window", "max_model_len", "context_length", "max_context_length"];
const OUTPUT_LIMITS = ["max_output_tokens", "max_completion_tokens", "max_tokens", "top_provider.max_completion_tokens"];

function listModels(req: ListModelsRequest) {
  const p = req.params;
  let rows = models.slice();
  for (const [col, f] of Object.entries(p?.filter ?? {})) {
    rows = rows.filter((m) => {
      const get = (): string => {
        switch (col) {
          case "gatewayId":
            return m.gatewayId;
          case "upstream":
            return m.upstream;
          case "displayName":
            return m.displayName;
          case "alias":
            return m.alias;
          case "disabled":
            return m.disabled ? "true" : "false";
          case "inputLimit":
            return String(metaLimit(m, INPUT_LIMITS));
          case "outputLimit":
            return String(metaLimit(m, OUTPUT_LIMITS));
          case "cost":
            return String(modelMeta(m, "pricing.input") ?? "");
          default:
            throw new ConnectError(`unknown filter column "${col}"`, Code.InvalidArgument);
        }
      };
      return matchesCell(get(), f);
    });
  }
  rows = listWindow(rows, p, p?.sort ?? "", {
    id: (m) => Number(m.id),
    gatewayId: (m) => m.gatewayId,
    upstream: (m) => m.upstream,
    displayName: (m) => m.displayName,
    alias: (m) => m.alias,
    disabled: (m) => (m.disabled ? 1 : 0),
    inputLimit: (m) => metaLimit(m, INPUT_LIMITS),
    outputLimit: (m) => metaLimit(m, OUTPUT_LIMITS),
    cost: (m) => {
      const v = modelMeta(m, "pricing.input");
      return typeof v === "number" ? v : "";
    },
  }, "gatewayId");

  const total = rows.length;
  return create(ListModelsResponseSchema, { models: page(rows, p), total });
}

// refreshModels is a no-op in the mock: the counts mirror current state.
function refreshModels() {
  return create(RefreshModelsResponseSchema, {
    providers: providers.length,
    models: models.length,
    warnings: [],
  });
}

function updateModel(req: UpdateModelRequest) {
  const m = models.find((x) => x.id === req.id);
  if (!m) throw new ConnectError(`model ${req.id} not found`, Code.NotFound);
  if (req.disabled !== undefined) m.disabled = req.disabled;
  if (req.alias !== undefined) {
    const alias = req.alias.trim();
    if (alias && models.some((x) => x.id !== req.id && x.gatewayId === alias)) {
      throw new ConnectError("alias is already another model's gateway ID", Code.AlreadyExists);
    }
    m.alias = alias;
    m.gatewayId = alias || `${m.upstream}/${m.upstreamModelId}`;
  }
  return create(ModelSchema, { ...m });
}

function deleteModel(req: DeleteModelRequest) {
  const i = models.findIndex((x) => x.id === req.id);
  if (i < 0) throw new ConnectError(`model ${req.id} not found`, Code.NotFound);
  models.splice(i, 1);
  return create(EmptySchema);
}

// ---- usage & requests ----

// The same seed data the dev REST mock showed before it was removed. One
// transcript has no key (a deleted key) to exercise the empty-key placeholder.
interface UsageSeed {
  gatewayModel: string;
  requests: number;
  promptTokens: number;
  cachedTokens: number;
  completionTokens: number;
  costUSD: number;
  createdAt: Date;
}
const usageSeeds: UsageSeed[] = [
  {
    gatewayModel: "hyper/hyperbolic-70b",
    requests: 2,
    promptTokens: 330,
    cachedTokens: 118,
    completionTokens: 115,
    costUSD: 0.0028,
    createdAt: new Date("2026-09-14T10:02:00Z"),
  },
];

interface RequestSeed {
  id: bigint;
  conversationId: string;
  keyName: string;
  gatewayModel: string;
  status: number;
  promptTokens: number;
  cachedTokens: number;
  completionTokens: number;
  costUSD: number | null;
  createdAt: Date;
  durationMs: number | null;
}
const requestSeeds: RequestSeed[] = [
  {
    id: BigInt(1), conversationId: "conv-001", keyName: "web",
    gatewayModel: "hyper/hyperbolic-70b", status: 200,
    promptTokens: 120, cachedTokens: 0, completionTokens: 84,
    costUSD: 0.0021, createdAt: new Date("2026-09-14T10:02:00Z"), durationMs: 2100,
  },
  {
    id: BigInt(2), conversationId: "conv-001", keyName: "web",
    gatewayModel: "hyper/hyperbolic-70b", status: 200,
    promptTokens: 210, cachedTokens: 118, completionTokens: 31,
    costUSD: 0.0007, createdAt: new Date("2026-09-14T10:03:00Z"), durationMs: 730,
  },
  {
    id: BigInt(3), conversationId: "conv-002", keyName: "",
    gatewayModel: "hyper/hyperbolic-70b", status: 429,
    promptTokens: 90, cachedTokens: 0, completionTokens: 0,
    costUSD: null, createdAt: new Date("2026-09-14T11:47:00Z"), durationMs: 45,
  },
];

// usageRows are the seed rows keyed to the mock's provider, flattened per the
// summary shape (one row per model).
const usageRowCache = usageSeeds;

// applyUsage narrows rows by the shared filter: [from, to] is closed; the
// keys list matches live key names only, includeDeletedKeys adds the
// deleted-key rows (keyName empty in the seed).
function usageWindow<T extends { createdAt: Date; keyName?: string }>(
  rows: T[],
  f: UsageFilter | undefined,
): T[] {
  const from = f?.from ? timestampDate(f.from) : null;
  const to = f?.to ? timestampDate(f.to) : null;
  const keys = f?.keys ?? [];
  return rows.filter(
    (r) =>
      (!from || r.createdAt >= from) &&
      (!to || r.createdAt <= to) &&
      (keys.length === 0 || r.keyName === undefined || keys.includes(r.keyName)),
  );
}

function getUsage(req: GetUsageRequest) {
  const kept = usageWindow(usageRowCache, req.filter);
  // Group by model, then apply the summary column filters and sort.
  const grouped = new Map<string, UsageSeed>();
  for (const r of kept) {
    const g = grouped.get(r.gatewayModel) ?? {
      gatewayModel: r.gatewayModel,
      requests: 0,
      promptTokens: 0,
      cachedTokens: 0,
      completionTokens: 0,
      costUSD: 0,
      createdAt: r.createdAt,
    };
    g.requests += r.requests;
    g.promptTokens += r.promptTokens;
    g.cachedTokens += r.cachedTokens;
    g.completionTokens += r.completionTokens;
    g.costUSD += r.costUSD;
    grouped.set(r.gatewayModel, g);
  }
  let rows = [...grouped.values()];
  for (const [col, f] of Object.entries(req.params?.filter ?? {})) {
    if (col !== "model") {
      throw new ConnectError(`unknown filter column "${col}"`, Code.InvalidArgument);
    }
    rows = rows.filter((r) => matchesCell(r.gatewayModel, f));
  }
  rows = listWindow(rows, req.params, req.params?.sort ?? "", {
    model: (r) => r.gatewayModel,
    requests: (r) => r.requests,
    prompt: (r) => r.promptTokens,
    cached: (r) => r.cachedTokens,
    completion: (r) => r.completionTokens,
    cost: (r) => r.costUSD,
  }, "model");

  const totals = {
    requests: kept.reduce((n, r) => n + r.requests, 0),
    promptTokens: kept.reduce((n, r) => n + r.promptTokens, 0),
    cachedTokens: kept.reduce((n, r) => n + r.cachedTokens, 0),
    completionTokens: kept.reduce((n, r) => n + r.completionTokens, 0),
    costUSD: kept.reduce((n, r) => n + r.costUSD, 0),
  };
  return create(GetUsageResponseSchema, {
    rows: page(rows, req.params).map((r) =>
      create(UsageRowSchema, {
        gatewayModel: r.gatewayModel,
        requests: BigInt(r.requests),
        promptTokens: BigInt(r.promptTokens),
        cachedTokens: BigInt(r.cachedTokens),
        completionTokens: BigInt(r.completionTokens),
        costUsd: r.costUSD,
      })),
    total: BigInt(rows.length),
    totals: {
      requests: BigInt(totals.requests),
      promptTokens: BigInt(totals.promptTokens),
      cachedTokens: BigInt(totals.cachedTokens),
      completionTokens: BigInt(totals.completionTokens),
      costUsd: totals.costUSD,
    },
  });
}

// requestCell renders a requests-table column for the filter ops.
function requestCell(r: RequestSeed, col: string): string {
  switch (col) {
    case "key":
      return r.keyName;
    case "model":
      return r.gatewayModel;
    case "status":
      return String(r.status);
    case "prompt":
      return String(r.promptTokens);
    case "cached":
      return String(r.cachedTokens);
    case "completion":
      return String(r.completionTokens);
    case "cost":
      return r.costUSD === null ? "" : String(r.costUSD);
    default:
      throw new ConnectError(`unknown filter column "${col}"`, Code.InvalidArgument);
  }
}

const requestSorts: Record<string, (r: RequestSeed) => string | number> = {
  id: (r) => Number(r.id),
  time: (r) => r.createdAt.getTime(),
  key: (r) => r.keyName,
  model: (r) => r.gatewayModel,
  status: (r) => r.status,
  prompt: (r) => r.promptTokens,
  cached: (r) => r.cachedTokens,
  completion: (r) => r.completionTokens,
  cost: (r) => r.costUSD ?? -1,
  duration: (r) => r.durationMs ?? -1,
};

function listRequests(req: ListRequestsRequest) {
  let rows = usageWindow(requestSeeds, req.filter).slice();
  // Default order: newest first (the store's tiebreak direction).
  rows.sort((a, b) => Number(b.id) - Number(a.id));
  for (const [col, f] of Object.entries(req.params?.filter ?? {})) {
    rows = rows.filter((r) => matchesCell(requestCell(r, col), f));
  }
  rows = listWindow(rows, req.params, req.params?.sort ?? "", requestSorts, "id");
  const total = BigInt(rows.length);
  return create(ListRequestsResponseSchema, {
    requests: page(rows, req.params).map((r) => ({
      id: r.id,
      conversationId: r.conversationId,
      keyName: r.keyName,
      gatewayModel: r.gatewayModel,
      status: r.status,
      promptTokens: BigInt(r.promptTokens),
      cachedTokens: BigInt(r.cachedTokens),
      completionTokens: BigInt(r.completionTokens),
      costUsd: r.costUSD === null ? undefined : r.costUSD,
      createdAt: timestampFromDate(r.createdAt),
      duration: r.durationMs === null
        ? undefined
        : durationFromMs(r.durationMs),
    })),
    total,
  });
}

// The conversation detail of transcript 1, for the dev request panel.
const requestDetailSeeds: Record<number, RequestDetail> = {
  1: create(RequestDetailSchema, {
    id: BigInt(1),
    conversationId: "conv-001",
    gatewayModel: "hyper/hyperbolic-70b",
    upstreamModel: "glm-4.6",
    status: 200,
    createdAt: timestampFromDate(new Date("2026-09-14T10:02:00Z")),
    completedAt: timestampFromDate(new Date("2026-09-14T10:02:02.100Z")),
    duration: durationFromMs(2100),
    contentStored: true,
    messages: [
      { role: "system", content: "You are a concise assistant." },
      { role: "user", content: "What is an LLM gateway?" },
      {
        role: "assistant",
        reasoning: "Keep it short.",
        content: "An LLM gateway is a central layer that proxies requests to one or more model providers.",
        finishReason: "stop",
      },
    ],
  }),
};

function getRequest(req: GetRequestRequest) {
  const detail = requestDetailSeeds[Number(req.id)];
  if (!detail) {
    throw new ConnectError(`request ${req.id} not found`, Code.NotFound);
  }
  return detail;
}

// ---- profiles ----

// kf builds a proto KeyFilter for the mock seeds.
function kf(
  mode: "none" | "include" | "exclude",
  values: string[] = [],
): KeyFilterProto {
  return create(KeyFilterSchema, {
    mode:
      mode === "include"
        ? KeyFilter_Mode.INCLUDE
        : mode === "exclude"
          ? KeyFilter_Mode.EXCLUDE
          : KeyFilter_Mode.NONE,
    values,
  });
}

interface ProfileSeed {
  name: string;
  providerFilter: KeyFilterProto;
  modelFilter: KeyFilterProto;
  parents: string[];
  isDefault: boolean;
}

// The same seed data the dev REST mock showed before it was removed. Counts
// are computed live from the mock's virtual keys (and child profiles).
const profileSeeds: ProfileSeed[] = [
  {
    name: "All",
    providerFilter: kf("none"),
    modelFilter: kf("none"),
    parents: [],
    isDefault: true,
  },
  {
    name: "hyper-chat",
    providerFilter: kf("include", ["hyper"]),
    modelFilter: kf("exclude", ["hyper/deepseek-v3"]),
    parents: [],
    isDefault: false,
  },
  {
    name: "hyper-strict",
    providerFilter: kf("none"),
    modelFilter: kf("none"),
    parents: ["hyper-chat"],
    isDefault: false,
  },
];

function profileOut(p: ProfileSeed): Profile {
  return create(ProfileSchema, {
    name: p.name,
    providerFilter: p.providerFilter,
    modelFilter: p.modelFilter,
    parents: p.parents,
    isDefault: p.isDefault,
    keyCount: keys.filter((k) => k.profile === p.name).length,
    childCount: profileSeeds.filter((x) => x.parents.includes(p.name)).length,
  });
}

// findProfileSeed resolves a seed or throws not_found, like the server.
function findProfileSeed(name: string): ProfileSeed {
  const p = profileSeeds.find((x) => x.name === name);
  if (!p) {
    throw new ConnectError(`profile "${name}" not found`, Code.NotFound);
  }
  return p;
}

function listProfiles(req: ListProfilesRequest) {
  let rows = profileSeeds.slice();
  for (const [col, f] of Object.entries(req.params?.filter ?? {})) {
    if (col !== "name") {
      throw new ConnectError(`unknown filter column "${col}"`, Code.InvalidArgument);
    }
    rows = rows.filter((r) => matchesCell(r.name, f));
  }
  // The default order is the read-only "All" profile first, then by name.
  rows = listWindow(rows, req.params, req.params?.sort ?? "", {
    id: (_r) => 0,
    name: (r) => (r.isDefault ? "" : r.name),
    keys: (r) => keys.filter((k) => k.profile === r.name).length,
  }, "name");
  return create(ListProfilesResponseSchema, {
    profiles: page(rows, req.params).map(profileOut),
    total: rows.length,
  });
}

// validateProfileMock mirrors the server's shape checks that the dev UI can
// hit (the XOR, empty include values); the store-side extras (unknown
// parents, cycles) are not simulated.
function validateProfileMock(
  name: string,
  providerFilter: KeyFilterProto | undefined,
  modelFilter: KeyFilterProto | undefined,
  parents: string[],
) {
  const provider = providerFilter ?? kf("none");
  const model = modelFilter ?? kf("none");
  if (name.trim() === "") {
    throw new ConnectError("name is required", Code.FailedPrecondition);
  }
  for (const [label, f] of [
    ["provider", provider],
    ["model", model],
  ] as const) {
    if (
      (f.mode === KeyFilter_Mode.INCLUDE || f.mode === KeyFilter_Mode.EXCLUDE) &&
      f.values.length === 0
    ) {
      throw new ConnectError(
        `${label} filter mode requires at least one value`,
        Code.FailedPrecondition,
      );
    }
  }
  if (
    parents.length > 0 &&
    (provider.mode === KeyFilter_Mode.INCLUDE ||
      provider.mode === KeyFilter_Mode.EXCLUDE ||
      model.mode === KeyFilter_Mode.INCLUDE ||
      model.mode === KeyFilter_Mode.EXCLUDE)
  ) {
    throw new ConnectError(
      "a derived profile has no filters of its own",
      Code.FailedPrecondition,
    );
  }
}

function createProfileMock(req: CreateProfileRequest): Profile {
  const name = req.name.trim();
  validateProfileMock(name, req.providerFilter, req.modelFilter, req.parents);
  if (profileSeeds.some((p) => p.name === name)) {
    throw new ConnectError(`profile "${name}" already exists`, Code.AlreadyExists);
  }
  const seed: ProfileSeed = {
    name,
    providerFilter: create(KeyFilterSchema, req.providerFilter),
    modelFilter: create(KeyFilterSchema, req.modelFilter),
    parents: [...req.parents],
    isDefault: false,
  };
  profileSeeds.push(seed);
  return profileOut(seed);
}

function updateProfileMock(req: UpdateProfileRequest): Profile {
  const current = findProfileSeed(req.name);
  if (current.isDefault) {
    throw new ConnectError("the All profile is read-only", Code.FailedPrecondition);
  }
  const newName = (req.newName ?? req.name).trim();
  validateProfileMock(newName, req.providerFilter, req.modelFilter, req.parents);
  if (newName !== current.name && profileSeeds.some((p) => p.name === newName)) {
    throw new ConnectError(`profile "${newName}" already exists`, Code.AlreadyExists);
  }
  const oldName = current.name;
  current.name = newName;
  current.providerFilter = create(KeyFilterSchema, req.providerFilter);
  current.modelFilter = create(KeyFilterSchema, req.modelFilter);
  current.parents = [...req.parents];
  // Derived profiles reference parents by name, so keep them pointing at the
  // renamed profile.
  for (const x of profileSeeds) {
    x.parents = x.parents.map((parent) => (parent === oldName ? newName : parent));
  }
  return profileOut(current);
}

function deleteProfileMock(req: DeleteProfileRequest) {
  const p = findProfileSeed(req.name);
  if (p.isDefault) {
    throw new ConnectError("the All profile is read-only", Code.FailedPrecondition);
  }
  const children = profileSeeds.filter((x) => x.parents.includes(req.name));
  if (children.length > 0) {
    throw new ConnectError(
      `profile is in use as a parent by ${children.length} profile(s)`,
      Code.FailedPrecondition,
    );
  }
  const usedByKeys = keys.filter((k) => k.profile === req.name).length;
  if (usedByKeys > 0) {
    throw new ConnectError(
      `profile is in use by ${usedByKeys} key(s)`,
      Code.FailedPrecondition,
    );
  }
  profileSeeds.splice(profileSeeds.indexOf(p), 1);
  return create(EmptySchema, {});
}

// ---- usage series & filter values ----

// SeriesEvent is one chartable usage event, derived from the mock's request
// seeds (each recorded request is one usage event).
interface SeriesEvent {
  model: string;
  keyName: string;
  upstream: string;
  at: Date;
  prompt: number;
  completion: number;
  cached: number;
  cost: number;
}
const seriesEvents: SeriesEvent[] = requestSeeds.map((r) => ({
  model: r.gatewayModel,
  keyName: r.keyName,
  upstream: "hyper",
  at: r.createdAt,
  prompt: r.promptTokens,
  completion: r.completionTokens,
  cached: r.cachedTokens,
  cost: r.costUSD ?? 0,
}));

// usageWindowMock applies the shared filter's key narrowing: absent keys and
// includeDeletedKeys matches everything; includeDeletedKeys alone matches
// only deleted-key events (mirroring store.UsageFilter.where).
function usageWindowMock<T extends { keyName?: string }>(
  rows: T[],
  f: UsageFilter | undefined,
  at: (r: T) => Date,
): T[] {
  const from = f?.from ? timestampDate(f.from) : null;
  const to = f?.to ? timestampDate(f.to) : null;
  const keys = f?.keys ?? [];
  return rows.filter((r) => {
    if (from && at(r) < from) return false;
    if (to && at(r) > to) return false;
    const parts: boolean[] = [];
    if (keys.length > 0) parts.push(keys.includes(r.keyName ?? ""));
    if (f?.includeDeletedKeys) parts.push((r.keyName ?? "") === "");
    return parts.length === 0 || parts.some(Boolean);
  });
}

function getUsageSeriesMock(req: GetUsageSeriesRequest) {
  // Clamp the bucket width like the server; absent means one hour.
  let bucketSecs = Number(req.bucketSize?.seconds ?? 3600n);
  if (bucketSecs < 60) bucketSecs = 60;
  if (bucketSecs > 30 * 24 * 3600) bucketSecs = 30 * 24 * 3600;

  const events = usageWindowMock(seriesEvents, req.filter, (e) => e.at);
  if (events.length === 0) {
    return create(GetUsageSeriesResponseSchema, {});
  }

  const label = (e: SeriesEvent): string => {
    switch (req.groupBy) {
      case SeriesGrouping.MODEL:
        return e.model;
      case SeriesGrouping.KEY:
        return e.keyName;
      case SeriesGrouping.UPSTREAM:
        return e.upstream;
      default:
        return "";
    }
  };

  // Bucket grid aligned to UTC multiples of the bucket width.
  const times = events.map((e) => e.at.getTime());
  const first =
    Math.floor(Math.min(...times) / (bucketSecs * 1000)) * bucketSecs * 1000;
  const last =
    Math.floor(Math.max(...times) / (bucketSecs * 1000)) * bucketSecs * 1000;
  const bucketCount = Math.round((last - first) / (bucketSecs * 1000)) + 1;

  const groups = new Map<string, UsageSeriesPoint[]>();
  const totals = {
    requests: 0,
    promptTokens: 0,
    cachedTokens: 0,
    completionTokens: 0,
    costUSD: 0,
  };
  for (const e of events) {
    const idx = Math.round(
      (Math.floor(e.at.getTime() / (bucketSecs * 1000)) * bucketSecs * 1000 - first) /
        (bucketSecs * 1000),
    );
    const lab = label(e);
    let points = groups.get(lab);
    if (!points) {
      points = Array.from({ length: bucketCount }, (_, i) =>
        create(UsageSeriesPointSchema, {
          bucketStart: timestampFromDate(new Date(first + i * bucketSecs * 1000)),
          requests: 0n,
          promptTokens: 0n,
          completionTokens: 0n,
          cachedTokens: 0n,
          reasoningTokens: 0n,
          costUsd: 0,
        }));
      groups.set(lab, points);
    }
    const p = points[idx];
    if (!p) continue;
    p.requests += 1n;
    p.promptTokens += BigInt(e.prompt);
    p.completionTokens += BigInt(e.completion);
    p.cachedTokens += BigInt(e.cached);
    p.costUsd += e.cost;
    totals.requests += 1;
    totals.promptTokens += e.prompt;
    totals.completionTokens += e.completion;
    totals.cachedTokens += e.cached;
    totals.costUSD += e.cost;
  }

  // Rank by total cost, trim to topGroups with an "Other" remainder.
  let entries = [...groups.entries()].sort((a, b) => {
    const cost = (ps: UsageSeriesPoint[]) => ps.reduce((n, p) => n + p.costUsd, 0);
    return cost(b[1]) - cost(a[1]);
  });
  const top = req.topGroups ?? 0;
  if (top > 0 && entries.length > top) {
    const rest = entries.slice(top);
    const firstRest = rest[0];
    const other: UsageSeriesPoint[] = firstRest
      ? firstRest[1].map((p) => create(UsageSeriesPointSchema, p))
      : [];
    for (const [, ps] of rest.slice(1)) {
      ps.forEach((p, i) => {
        const o = other[i];
        if (!o) return;
        o.requests += p.requests;
        o.promptTokens += p.promptTokens;
        o.completionTokens += p.completionTokens;
        o.cachedTokens += p.cachedTokens;
        o.costUsd += p.costUsd;
      });
    }
    entries = [...entries.slice(0, top), ["Other", other]];
  }

  return create(GetUsageSeriesResponseSchema, {
    series: entries.map(([lab, points]) => ({ label: lab, points })),
    totals: {
      requests: BigInt(totals.requests),
      promptTokens: BigInt(totals.promptTokens),
      cachedTokens: BigInt(totals.cachedTokens),
      completionTokens: BigInt(totals.completionTokens),
      costUsd: totals.costUSD,
    },
  });
}

function listFilterValuesMock(req: ListFilterValuesRequest) {
  const q = req.query.toLowerCase();
  const limit = req.limit === undefined || req.limit <= 0 ? 100 : req.limit;
  let values: string[];
  switch (req.column) {
    case "model":
      values = [...new Set(requestSeeds.map((r) => r.gatewayModel))].sort();
      break;
    case "key":
      values = keys.map((k) => k.name).sort();
      break;
    default:
      throw new ConnectError(
        `unknown filter column "${req.column}"`,
        Code.InvalidArgument,
      );
  }
  return create(ListFilterValuesResponseSchema, {
    values: values.filter((v) => v.toLowerCase().includes(q)).slice(0, limit),
  });
}

// ---- settings ----

// The same seed value the dev REST mock used.
let storePrompts = true;

function getSettings() {
  return create(SettingsSchema, { storePrompts });
}

function updateSettings(req: UpdateSettingsRequest) {
  if (req.storePrompts !== undefined) storePrompts = req.storePrompts;
  return create(SettingsSchema, { storePrompts });
}

// ---- config export ----

// A small static document; the dev export is a shape preview, not live state.
function exportConfig() {
  return create(ExportConfigResponseSchema, {
    yaml:
      "upstreams:\n" +
      "  - name: default\n" +
      "    url: http://zeph:9931/v1\n" +
      "    api_key_env: DEFAULT_API_KEY\n" +
      "    refresh_interval: 5m0s\n",
  });
}
