# Admin API → ConnectRPC migration plan

Branch: `feat/admin-connect-migration`.
Phase 0 (schema + committed codegen) is done: `proto/toll/admin/v1`,
`gen/`, `web/src/gen/`, `buf.*.yaml`. See `proto/README.md` for the
deliberate behavior changes this plan implements — they are product
decisions, not transcription choices.

Strategy: **strangle, don't flip.** The connect handler and the legacy
REST mux coexist under `/admin`; each resource cuts over independently
(server impl → SPA view → legacy route deletion). The SPA's only
consumer is our own bundle, so no external compatibility window is
needed — but legacy routes are deleted in the *same* phase as their
view flips, never left as dead code.

## Phase 1 — connect server skeleton + `keys` pilot (server)

- Mount connect-go in `internal/admin`:
  - `admin.Handler` builds an `http.ServeMux` as today; add
    `mux.Handle("/admin/api/toll.admin.v1.AdminService/",
    adminv1connect.NewAdminServiceHandler(svc, opts))` alongside the
    existing REST routes. Everything not yet migrated keeps working.
  - New `internal/admin/connect/` (or `internal/admin/svc.go`):
    `type service struct{ store, logger, ... }` implementing
    `adminv1connect.AdminServiceHandler`.
- **Error mapping helper** (one place, per the conventions in
  `admin.proto`): store sentinels → `connect.Error` codes
  (`ErrBadListParam→invalid_argument`, `ErrKeyNotFound→not_found`,
  `ErrAliasConflict→already_exists`, `ErrProfile*→failed_precondition`
  …). Every resource phase routes through it.
- **Store changes the keys pilot needs** (`internal/store/keys.go`):
  - `RevokeVirtualKey` / `PauseVirtualKey`: check `RowsAffected`,
    return `ErrKeyNotFound` (today they silently no-op — README item 5).
  - `RotateVirtualKey(ctx, name) (plaintext string, err)`: new
    `keys.Generate()` + hash update, name/profile untouched.
  - `UpdateVirtualKey`: PATCH semantics — only apply fields the
    request marks present (README item 3).
- **Wire mapping helpers** (`internal/admin/wire.go` or per-resource):
  proto ↔ store types; `ListParams` ↔ `store.ListParams` (incl.
  `ColumnFilter{conditions,join}` ↔ the extended spec type below).
- **Tests**: extend `admin_test.go` style — `httptest` against the
  connect handler with Connect-protocol JSON bodies; cover: list
  paging/sort/filter, create returns key+plaintext once, update
  absent-fields-unchanged, rotate invalidates old secret, revoke of
  unknown key → not_found.

## Phase 2 — `keys` pilot (SPA)

- `web/src/lib/connect.ts`: build the client once.
  - Prod: `createClient(AdminService, { transport: createConnect({
    baseUrl: '/admin/api', useBinaryFormat: true }) })`.
  - Dev (`bun run dev`): in-memory transport backed by a mock service
    implementation (`@connectrpc/connect` `createRouter` + a fake
    `Transport`, or `createClient(AdminService, mockImpl)`), replacing
    the hand-written `fetch` mocks in `vite.config.ts` for this
    resource. The mock implements the **generated interface**, so it
    can no longer drift — that is the payoff of the whole migration.
- Flip `web/src/views/Keys.tsx` + `web/src/lib/api.ts` to the typed
  client; delete the key-shaped types from `web/src/lib/types.ts`.
- UI additions the schema enables: Rotate button (one-time plaintext
  dialog shared with create), `UpdateKey` patch dialog (pause toggle
  inline, rename + profile in one edit).
- Remove `keys` REST routes from `admin.go` and their mock handlers
  from `vite.config.ts` in the same commit.

## Phase 3 — remaining resources (server + SPA per resource)

Order by dependency: settings/config first (tiny), then
providers+models (they share the registry/sync surface), then
requests+usage (biggest query work).

### 3a. settings + config export
- `GetSettings` / `UpdateSettings` / `ExportConfig` (string YAML; SPA
  keeps building its own download Blob). No store changes.

### 3b. providers + models
- Store: none beyond wire mapping; `CreateProvider` response must
  construct the `Provider` post-sync (reachable/last_synced_at are new
  outputs — README).
- Toggle collapse: `UpdateProvider{disabled}`, `UpdateModel{disabled,
  alias}` replace four enable/disable routes + alias PUT.
- `RefreshModels` stays unary (comment in `admin.proto` documents the
  streaming escape hatch).

### 3c. requests + usage — the filter engine work
- **`store.FilterSpec` → multi-condition**: `buildFilters` gains the
  `ColumnFilter{conditions, join}` shape (recursive-free, one level),
  the validation rules from `common.proto` (zero conditions → 400,
  blank text-op values → 400, multi-condition requires join), and the
  numeric ops `GT/GTE/LT/LTE/BETWEEN` (`CAST(... AS REAL)`).
  This fixes the dropped-`condition2` bug at the same time:
  `translateFilters` in `useServerRows.ts` maps condition1+condition2
  +logic into the proto.
- **Deleted keys**: `key_name` returns empty (drop the SQL
  `COALESCE('(deleted key)')`); UI renders the placeholder;
  `include_deleted_keys` replaces the `__deleted__` sentinel in
  `parseFilter` and `Usage.tsx` (`DELETED_KEYS`).
- **int64**: token sums / request-table totals widen in `store` +
  handlers; SPA number formatting tolerates bigint/JSON-string ids
  (README item 7) — audit `format.ts`/table cells.
- Usage `from`/`to` stay a closed interval; proto `Timestamp` ↔
  `store.FormatTime` in the mapping layer.

## Phase 4 — legacy teardown

- Delete the remaining `mux.HandleFunc("GET /api/…")` REST routes,
  `parseFilter`/`parseListParams`, `writeJSON`/`readJSON`/`listError`
  helpers, the legacy mock block in `vite.config.ts`, and every
  migrated section of `web/src/lib/types.ts` (the file should end up
  holding only view-local types).
- `admin_test.go` fully rewritten against the connect handler;
  AGENTS.md "Admin API shapes live in three places" bullet becomes
  "shapes live in the proto; regenerate + wire tests" (the three-places
  landmine is retired).

## Phase 5 — new surface (post-cutover, independent)

- `ListFilterValues`: `SELECT DISTINCT gateway_model | vk.name FROM
  usage_events ... WHERE value LIKE ? LIMIT ?` — feeds set-filter
  dropdowns type-ahead.
- `GetUsageSeries`: store query bucketing
  `strftime`-style on `created_at` (UTC-aligned to `bucket_size`),
  optional group by model/key/upstream, dense zero-filled buckets,
  `top_groups` remainder as "Other", `compare_to_previous` = second
  window. Then the charts UI (Carbon `StackedAreaChart`/`LineChart`).
- Key rotation cooldown / budget-style extras: revisit only if real
  demand appears.

## Phase 6 — CI + hygiene

- `.github/workflows`: add a job running `buf lint`, `buf build`, and
  `buf generate` + `git diff --exit-code gen web/src/gen` (codegen
  drift gate) and `buf breaking --against` the default branch once
  v1 is released.
- Nix: nothing to change (generated code is committed; buf stays a
  dev-only tool via bunx).
- Keep `proto/README.md`'s behavior-change list in sync when the
  schema evolves; new fields/RPCs only (field numbers are forever).

## Definition of done

`toll` serves `/admin/api/*` exclusively as ConnectRPC (JSON or binary
from the same handler), the SPA contains zero hand-written API types
for migrated surfaces, dev mocks implement generated interfaces, every
README behavior change has a test pinning it, and CI rejects schema
drift or breaking changes.
