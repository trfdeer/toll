# toll admin API schema

The `toll.admin.v1` ConnectRPC service: the planned replacement for the
hand-written JSON admin API in `internal/admin`. Conventions (JSON naming,
timestamps, error codes, secrets) are documented in
[`toll/admin/v1/admin.proto`](toll/admin/v1/admin.proto).

## Tooling

- `buf lint` / `buf build` from the repo root (module config in `buf.yaml`).
- `buf generate` regenerates Go into `gen/` and TypeScript into
  `web/src/gen/`. **Generated code is committed**; `go build ./...`,
  `bun run build` and the Nix packages never invoke buf.

## Deliberate behavior changes vs today's HTTP API

These are product decisions, not transcription errors — don't "fix" them
back without updating this list (and the tests that pin them):

1. **Toggle verbs collapse into Updates.** `enable`/`disable`/`pause`/
   `resume`/alias-PUT become `optional` fields on `Update{Provider,Model,
   Key}`. `RevokeKey` stays a named method (one-way), `RefreshModels`
   stays a job.
2. **`__deleted__` sentinel is gone.** Filtering deleted-key events uses
   `UsageFilter.include_deleted_keys`; deleted keys render as an empty
   `RequestSummary.key_name` (the UI shows the placeholder). A key actually
   *named* `__deleted__` now behaves like any other name.
3. **`PUT /api/keys/{name}` becomes PATCH-like.** Absent fields on
   `UpdateKey` are left unchanged (today an omitted profile silently
   resets the key to "All"; here, empty-string profile does that
   explicitly).
4. **Creates/updates return the resource** instead of 204/echo bodies;
   `CreateKeyResponse` carries the `VirtualKey` next to the one-time
   plaintext.
5. **`Revoke`/`Update` on unknown keys return `not_found`** — today's
   store silently no-ops; the Connect implementation must add the
   `RowsAffected` check.
6. **Timestamps/durations are well-known types** (RFC3339 / `"1.5s"` in
   JSON), replacing string timestamps and `*_ms`/`*_seconds` ints.
7. **Token sums and request-table totals are `int64`** (int32 wraps at
   ~2.1B tokens); int64 arrives as `bigint`/JSON-string in connect-es.
8. **Malformed filters fail loudly**: zero-condition column filters,
   blank-valued text ops, and multi-condition filters without an explicit
   join are `invalid_argument` (today they silently match all/nothing);
   `limit > 1000` is rejected, not clamped.
9. **Usage responses drop `totalReqs` and the pre-formatted `totalCost`
   string** (the SPA reads neither); `from`/`to` remain a **closed**
   interval.
10. **Config export returns the YAML as a string field** (the SPA already
    builds its own download Blob).
11. **New surface:** `GetUsageSeries` (charts), `ListFilterValues`
    (server-fed dropdowns), `RotateKey`, and numeric filter ops
    (`GT/GTE/LT/LTE/BETWEEN`) for the tables' number/date columns.
