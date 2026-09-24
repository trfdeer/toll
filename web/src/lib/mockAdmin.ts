// In-memory implementation of the migrated toll.admin.v1 AdminService RPCs
// for the Vite dev server. The routes are registered per method on the dev
// router, so an unimplemented RPC fails exactly like the real server's
// strangled surface does, and each implementation is type-checked against the
// generated method descriptor — the mock cannot drift from the schema.
import { create } from "@bufbuild/protobuf";
import { EmptySchema } from "@bufbuild/protobuf/wkt";
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

// matches applies one column filter to a row, mirroring the server's text-op
// semantics (case-insensitive, "" matches blank cells).
function matches(k: VirtualKey, col: string, f: ColumnFilter): boolean {
  if (f.conditions.length === 0) {
    throw new ConnectError("at least one condition is required", Code.InvalidArgument);
  }
  const s = cell(k, col);
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

// listKeys mirrors the Go list endpoint: ANDed per-column filters, then
// sort (with the id tiebreak), then paging (absent limit = 50, 0 = all).
function listKeys(req: ListKeysRequest) {
  const p = req.params;
  let rows = keys.slice();
  for (const [col, f] of Object.entries(p?.filter ?? {})) {
    rows = rows.filter((k) => matches(k, col, f));
  }
  const sort = p?.sort || "name";
  const dir = p?.dir === SortDirection.DESC ? -1 : 1;
  const accessors: Record<string, (k: VirtualKey) => string | number> = {
    id: (k) => keys.indexOf(k),
    name: (k) => k.name,
    profile: (k) => k.profile,
    revoked: (k) => (k.revoked ? 1 : 0),
    paused: (k) => (k.paused ? 1 : 0),
  };
  const acc = accessors[sort];
  if (!acc) {
    throw new ConnectError(`unknown sort column "${sort}"`, Code.InvalidArgument);
  }
  rows.sort((a, b) => {
    const av = acc(a);
    const bv = acc(b);
    const cmp =
      typeof av === "number" && typeof bv === "number"
        ? av - bv
        : String(av).localeCompare(String(bv), undefined, { numeric: true });
    return cmp * dir || (keys.indexOf(a) - keys.indexOf(b)) * dir;
  });

  const total = rows.length;
  const limit = p?.limit === undefined ? 50 : p.limit;
  const offset = p?.offset ?? 0;
  rows = limit > 0 ? rows.slice(offset, offset + limit) : rows.slice(offset);
  return create(ListKeysResponseSchema, { keys: rows, total });
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
