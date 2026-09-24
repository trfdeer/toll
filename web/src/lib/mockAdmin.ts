// In-memory implementation of the migrated toll.admin.v1 AdminService RPCs
// for the Vite dev server. The routes are registered per method on the dev
// router, so an unimplemented RPC fails exactly like the real server's
// strangled surface does, and each implementation is type-checked against the
// generated method descriptor — the mock cannot drift from the schema.
import { create } from "@bufbuild/protobuf";
import {
  EmptySchema,
  timestampDate,
  timestampFromDate,
} from "@bufbuild/protobuf/wkt";
import type { JsonObject } from "@bufbuild/protobuf";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";
import { AdminService } from "../gen/toll/admin/v1/admin_pb";
import {
  FilterOp,
  SortDirection,
  type ColumnFilter,
} from "../gen/toll/admin/v1/common_pb";
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
import type { ListParams } from "../gen/toll/admin/v1/common_pb";

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
