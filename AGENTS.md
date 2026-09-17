# AGENTS.md

## Tooling rules

- Never use raw bash scripts or raw HTTP requests where an MCP or dedicated tool is available for the job. Use the provided tools/MCPs instead.
- Never, under any circumstances, browse or read the source code of a library to figure out how it works. Use the official documentation and available MCPs instead.
- Never open or view screenshots yourself. Start a subagent running the `glm-5.3-flash` model, and have it inspect the screenshot, describe what it sees, and answer any questions about it.

## What this is

`toll` — a minimal, self-hosted LLM gateway for OpenAI-compatible APIs. One Go module (`github.com/trfdeer/toll`) plus a React/Vite admin SPA that is embedded into the Go binary. No monorepo tooling. (Renamed from `gate`; "gateway" remains the domain term — don't rename it.)

- Entrypoint: `cmd/toll/main.go`. `toll` runs the server; also `toll keys add|list|revoke` and `toll healthcheck`.
- Key packages: `internal/config` (load), `store` (SQLite + migrations), `discovery` (model sync), `registry` (alias/overlay engine), `proxy` (usage-capturing proxy for `/v1/chat/completions` and `/v1/responses`), `api` (auth + `/v1/models`), `admin` (admin API + embedded UI), `wire` (OpenAI JSON parse/rewrite), `cost`, `keys`, `server`, `e2e`.
- `internal/admin/web/dist` is embedded via `//go:embed all:web/dist`; a tracked `.gitkeep` keeps `go build` working on a clean checkout.

## Commands

Go (repo root; SQLite is pure-Go, no CGO, tests need no external services):
- Build: `go build ./...`
- Test all: `go test ./...`
- One package / one test: `go test ./internal/proxy -run TestCaptureStreaming`
- Vet: `go vet ./...`

Web (`web/` uses **bun** — `bun.lock`, not npm/yarn):
- `cd web && bun install`
- `bun run dev` — Vite dev server only; it mocks the entire `/admin/api` (see `web/vite.config.ts`), so no Go server is needed for UI work.
- `bun run typecheck` (`tsc --noEmit`); `bun run build` runs typecheck then `vite build`.
- Vite `outDir` is `../internal/admin/web/dist`, i.e. the bundle the Go binary embeds. Run `bun run build` before `go build` to refresh the embedded UI.

Nix (dendritic flake-parts layout — outputs are auto-imported from `nix/`; add a file there instead of editing `flake.nix`):
- `nix build .#toll`, `nix build .#web`, `nix build .#image` (image is built by Nix, no Docker daemon: `docker load < result` → local image `toll:<version>`).
- `nix fmt` formats via nixfmt-tree.
- Dependency hashes live in `nix/hashes.json`; refresh with the commands in the comments in `nix/packages.nix` / `nix/web.nix` (e.g. `nix build .#web-node-modules-updater 2>&1 | grep 'got:'`).

## Config & runtime

- Precedence: flags > `TOLL_*` env > `toll.yaml` > defaults. Secrets are **never** in the file — each upstream names an env var via `api_key_env`.
- `./toll.yaml` is picked up automatically; see `examples/toll.yaml` for the full surface. Zero upstreams is valid: providers can be added at runtime through the admin UI.
- State is SQLite at `$TOLL_DATA_DIR/toll.db` (default `./data`), plus prompt/response bodies in `$TOLL_DATA_DIR/content.db`. Local dev DBs under `data/` are not gitignored — don't commit them.

## Things that are easy to get wrong

- **`/admin` is served without authentication** (`internal/admin/admin.go`). Bind it to a trusted interface or front it with auth; `/v1/*` is the authenticated surface.
- **Migrations**: append a new SQL string to the `migrations` slice in `internal/store/store.go`; never edit an applied migration. `store.Open` applies pending ones at startup.
- **Admin API shapes live in three places**: `internal/admin/admin.go`, the Vite mock in `web/vite.config.ts`, and `web/src/lib/types.ts`. Changing an endpoint means updating all three.
- **Alias/registry resolution** (`internal/registry`): gateway ID = explicit config entry alias > first matching `alias_rules` > `{upstream name}/{upstream model id}`. Overlays match the *gateway* ID; explicit entry metadata always wins. Cross-upstream gateway-ID collisions: the earlier config position wins.
- **Key filters gate on the discovery upstream name**, not a prefix parsed from the model ID (`keys.Allows`).
- **Cost** (`internal/cost`): metadata `pricing` is per-Mtok USD with keys `input`, `output`, `cache_create`, `cache_hit`. Missing rates yield a `nil` cost, never a guess; `TOLL_LOG_LEVEL=debug` also logs when the computed cost diverges from an upstream-reported one.
- **Config `disabled` is a `*bool`**: omit it to leave the registry's toggle alone; set it to pin the state across discovery refreshes.
- **Prompt bodies live in `content.db`** (`transcript_content`), never in `toll.db`; `transcripts` keeps only metadata. The `store_prompts` setting gates writes: the admin UI Settings toggle (DB) is authoritative at runtime, `TOLL_STORE_PROMPTS`/`store_prompts:` seed it at startup. When off, `Store.Transcript` returns empty bodies and the admin detail reports `contentStored: false`.
- **Model aliases** are DB state (`model_aliases`), not config-resolved. Config `models[].alias` seeds them once at startup; after that the admin UI wins (export the config to persist edits). `ReplaceModels` re-applies aliases on every discovery sync and keeps `models.base_gateway_id` so clearing restores the computed ID.
- **Deleted keys** show as a reserved `key=__deleted__` value in the usage/requests filters (`admin.deletedKeysSentinel`), mapping to `vk.name IS NULL`.

## Commit conventions

Commits follow Conventional Commits (`type(scope): subject`, imperative, lower case) with
Keep a Changelog bodies (`Added:` / `Changed:` / `Fixed:` … bullets describing what users
or operators observe). The scope must come from this list:

| Scope | Area |
| --- | --- |
| `proxy` | `internal/proxy` — usage-capturing upstream proxy |
| `registry` | `internal/registry` — alias/overlay engine |
| `discovery` | `internal/discovery` — model sync |
| `store` | `internal/store` — SQLite schema, migrations, queries |
| `admin` | `internal/admin` — admin API, export, transcripts |
| `api` | `internal/api` — auth + `/v1/models` |
| `config` | `internal/config` and `examples/toll.yaml` |
| `web` | `web/` — the admin SPA |
| `nix` | `flake.nix`, `nix/`, CI builds |

Use `cli` for `cmd/toll` and `wire`, `cost`, `keys`, `server` for those packages; omit the
scope when a change spans several areas, when it fits none of them, or for repo-wide
commits (docs, CI, initial import).

## Publishing

- Container images are released to **`ghcr.io/trfdeer/toll`** by `.github/workflows/release.yml` (builds `nix build .#image`, loads the docker archive, pushes `:<tag>` + `:latest` on `v*` tags; every push to `main` and every manual run publishes a `snapshot-<sha>` tag only, never `:latest`). The package inherits the repo's visibility until made public on ghcr.
- The Nix image itself is named plain `toll:<version>`; the registry path is applied in CI, not in `nix/packages.nix`. Images are **linux/amd64 only** (that is a deliberate product decision, not an unfixed gap).
