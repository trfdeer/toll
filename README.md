# toll

A minimal, self-hosted LLM gateway for OpenAI-compatible APIs.

Put `toll` in front of any number of OpenAI-compatible providers (OpenAI, OpenRouter, DeepSeek, vLLM, LM Studio, …) and your clients see one authenticated endpoint with one merged, aliased model catalog. Every request is proxied with live streaming, captured with token usage and computed cost, and inspectable in a built-in admin UI.

- **One binary.** A single Go executable with the admin UI embedded. SQLite for state — no Postgres, no Redis, no external services.
- **12-factor.** Config from env + an optional declarative YAML file. Secrets *never* live in the file — each upstream names an env var via `api_key_env`.
- **Honest proxying.** SSE chunks flush to the client as the upstream emits them; upstream errors pass through verbatim. No retries, no failover surprises.

## Quickstart

Run a gateway in front of a single upstream with nothing but the environment:

```sh
export TOLL_UPSTREAM_URL=https://api.openai.com/v1
export TOLL_UPSTREAM_API_KEY=sk-...
toll
```

Or use a config file (`./toll.yaml` is picked up automatically, or pass `-c`):

```yaml
listen: ":8080"

upstreams:
  - name: openrouter
    url: https://openrouter.ai/api/v1
    api_key_env: OPENROUTER_API_KEY
    refresh_interval: 5m
    alias_rules:
      - match: "^(.+)$"
        as: "openrouter/$1"

  - name: openai
    url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
    alias_rules:
      - match: "^(.+)$"
        as: "openai/$1"
```

```sh
export OPENROUTER_API_KEY=... OPENAI_API_KEY=...
toll
```

See [`examples/toll.yaml`](examples/toll.yaml) for the full surface, including per-model pins and metadata overlays.

Create a virtual API key for your clients:

```sh
$ toll keys add my-app
key created (store this now — it is not recoverable) name=my-app key=sk-tl-3f2a…
```

Point any OpenAI-compatible client at the gateway, authenticating with the virtual key:

```sh
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-tl-3f2a…" \
  -H "content-type: application/json" \
  -d '{"model": "openrouter/deepseek-v3.2", "messages": [{"role": "user", "content": "hi"}]}'
```

## How it works

### Model registry

Each upstream's `/v1/models` is discovered at startup and re-synced on a timer. Discovered models are rewritten into *gateway IDs*:

1. an explicit `models[].alias` entry, else
2. the first matching `alias_rules` template, else
3. `{upstream name}/{upstream model id}`

Metadata **overlays** deep-merge into upstream model metadata — they match the *gateway* ID and only inject or override, never delete. Explicit config entries always win over rules and overlays. Model aliases are DB state: config seeds them once, after that the admin UI is authoritative (export the config from the UI to persist edits across restarts).

### Proxy & usage capture

`POST /v1/chat/completions` and `POST /v1/responses` are routed by the requested model ID to the upstream that serves it. The client's virtual key is replaced with the upstream's API key — the upstream credential never reaches the client. Usage is parsed from both streaming and non-streaming responses (including reasoning/cached token accounting) and recorded with a computed cost.

Cost comes from a `pricing` object in model metadata (per-Mtok USD: `input`, `output`, `cache_create`, `cache_hit`). Missing rates yield an *unknown* cost, never a guess. At `TOLL_LOG_LEVEL=debug`, the gateway logs when its computed cost diverges from an upstream-reported one.

### Virtual keys

Clients authenticate with `sk-tl-…` keys (SHA-256 hashed at rest, plaintext shown once). Each key carries optional provider and model filters — include or exclude — checked against the upstream a model was discovered from, so aliasing can't smuggle a model past a filter. `GET /v1/models` is filtered per key.

```sh
toll keys add restricted --allow-provider openrouter --deny-model openrouter/some-pricey-model
toll keys list
toll keys revoke restricted
```

Keys can also be created, paused, resumed, edited, and deleted from the admin UI.

## Admin UI

The UI is served at **`/admin`** — no build step needed, it ships inside the binary:

- **Usage** — per-key/per-model token and cost rollups, request log with drill-down into individual requests (status, latency, usage, cost, and prompt/response content when storage is on).
- **Providers** — add, remove, enable, or disable upstreams at runtime; zero upstreams in config is a valid starting state.
- **Models** — browse the merged catalog, edit aliases, toggle models disabled, trigger discovery refresh.
- **Virtual Keys** — full key lifecycle and filter editing.
- **Settings** — prompt-content storage toggle; **config export** to YAML that round-trips through `toll -c`.

> **⚠️ `/admin` is served without authentication.** Bind the gateway to a trusted interface or front `/admin` with auth in your reverse proxy. `/v1/*` is the authenticated surface.

Prompt and response bodies are stored in a separate database (`content.db`) from the metadata (`toll.db`), so they can be kept, wiped, or never written independently. Disable storage entirely with `store_prompts: false` / `TOLL_STORE_PROMPTS=false`, or the UI toggle — when off, only metadata (tokens, cost, timing, status) is recorded.

## CLI

```
toll                          run the gateway
toll -c path.yaml -l :9090    explicit config file / listen address
toll keys add|list|revoke     manage virtual API keys
toll healthcheck              probe /healthz (used by the container HEALTHCHECK)
```

## Configuration reference

Precedence: **flags > `TOLL_*` env > `toll.yaml` > defaults**.

| Setting | Env | File | Default |
| --- | --- | --- | --- |
| Listen address | `TOLL_LISTEN` / `-l` | `listen` | `:8080` |
| Config file | `TOLL_CONFIG` / `-c` | — | `./toll.yaml` if present |
| Log level (`debug` logs cost divergence) | `TOLL_LOG_LEVEL` | `log_level` | `info` |
| Data directory (SQLite) | `TOLL_DATA_DIR` | `data_dir` | `./data` |
| Store prompt bodies | `TOLL_STORE_PROMPTS` | `store_prompts` | UI/DB value wins at runtime |
| Single upstream (no file) | `TOLL_UPSTREAM_URL`, `TOLL_UPSTREAM_API_KEY` | — | — |

Upstream entries take `name`, `url`, `api_key_env` (required — the env var is read at startup), `refresh_interval` (default `5m`), `disable_refresh`, `alias_rules`, `overlays`, and explicit `models`. A model entry's `disabled: true` hides it from `/v1/models` and refuses routing without removing it from the registry; omit the key to leave the UI's toggle alone across refreshes.

The gateway fails fast at startup listing *every* config problem at once.

## Endpoints

| Path | Auth | Purpose |
| --- | --- | --- |
| `GET /healthz` | — | liveness probe |
| `GET /v1/models` | virtual key | registry catalog, filtered per key |
| `POST /v1/chat/completions` | virtual key | routed proxy with usage capture |
| `POST /v1/responses` | virtual key | routed proxy with usage capture |
| `/v1/*` | virtual key | passthrough to the first configured upstream |
| `/admin` | **none** | admin UI + API (`/api/…`) |

## Install

**Container** — released on `v*` tags to `ghcr.io/trfdeer/toll` (`:<tag>` and `:latest`, linux/amd64):

```sh
docker run -d -p 8080:8080 -v toll-data:/data \
  -e TOLL_UPSTREAM_URL=https://openrouter.ai/api/v1 \
  -e TOLL_UPSTREAM_API_KEY=... \
  ghcr.io/trfdeer/toll:latest
```

(or mount your own config with `-v ./toll.yaml:/data/toll.yaml` — the image's workdir is `/data`, where `./toll.yaml` is auto-discovered).

**Nix** — the flake packages the gateway, the web bundle, and a container image:

```sh
nix build .#toll      # binary
nix build .#image     # docker archive: docker load < result
```

**Go** — build from source (Go ≥ 1.25; SQLite is pure-Go, no CGO needed):

```sh
cd web && bun install && bun run build && cd ..   # refresh the embedded UI
go build -o toll ./cmd/toll
```

## Development

```sh
go build ./...
go test ./...                      # no external services needed
go test ./internal/proxy -run TestCaptureStreaming
go vet ./...
```

UI work without the Go server:

```sh
cd web
bun install
bun run dev    # Vite dev server; mocks the whole /admin/api
```

The Vite build outputs into `internal/admin/web/dist`, which is `go:embed`-ed into the binary — run `bun run build` before `go build` to ship UI changes.

Layout: `cmd/toll` (entrypoint) and `internal/` packages — `config`, `store` (SQLite + migrations), `discovery` (model sync), `registry` (alias/overlay engine), `proxy` (usage-capturing proxy), `api` (auth + `/v1/models`), `admin` (admin API + embedded UI), `wire` (OpenAI JSON parse/rewrite), `cost`, `keys`, `server`, `e2e`.
