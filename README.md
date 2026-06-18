# opencode-sw

A multi-key management gateway for **OpenCode Go** services, written in Go + Gin and ready to deploy on **Zeabur**.

It aggregates many OpenCode Go API keys behind a single set of universal endpoints, so any OpenAI- / Anthropic-compatible client (including opencode itself) can consume them with one gateway token.

## Features

- 🔑 **Multi-key pool** with weighted round-robin scheduling, exponential-backoff failure cooldown, and usage counting
- 🔄 **Full cross-protocol conversion** — call any model from any endpoint:
  - Use `/v1/chat/completions` to talk to Claude (Messages protocol)
  - Use `/v1/messages` to talk to GPT-5 (Responses protocol)
  - Use `/v1/responses` to talk to DeepSeek (Chat protocol)
  - All conversions work with **streaming SSE** too
- 🧭 **Model routing table** mapping gateway model ids → real upstream model + protocol
- 🗺️ **Model Mapping** — optionally rewrite requested `model` names before upstream forwarding (with Admin UI and recalculated `Content-Length`)
- 🌐 **Universal endpoints**:
  - `POST /v1/chat/completions` — OpenAI Chat Completions
  - `POST /v1/messages` — Anthropic Messages
  - `POST /v1/responses` — OpenAI Responses API
  - `GET  /v1/models` — catalog discovery
- 🛡️ **Gateway token auth** (`Authorization: Bearer <token>`, also accepts `x-api-key`)
- ⏱️ **Per-token rate limiting** (sliding window, configurable req/min)
- 🖥️ **Web admin panel** (`/admin`) — Vue 3 SPA with dashboard, charts, KEY/Token/Model CRUD, dark/light theme, top bar with dropdown menus
- 🛠️ **Admin REST API** (`/admin/*`) for programmatic management
- 🗄️ **Embedded SQLite** (GORM), persisted to a Zeabur volume
- 🐳 Single-binary Docker image, one-click Zeabur deploy

## Architecture

```
Client (any protocol)
  │
  ▼
┌─────────────────────────────┐
│  opencode-sw gateway        │
│  ┌───────────────────────┐  │
│  │ Auth + Rate Limit     │  │
│  ├───────────────────────┤  │
│  │ Protocol Decoder      │  │  ← decodes inbound (Chat/Messages/Responses)
│  ├───────────────────────┤  │
│  │ IR (Intermediate Rep) │  │  ← unified request/response format
│  ├───────────────────────┤  │
│  │ Protocol Encoder      │  │  ← encodes to upstream protocol
│  ├───────────────────────┤  │
│  │ Weighted Key Picker   │  │  ← round-robin with exponential backoff
│  ├───────────────────────┤  │
│  │ Upstream Proxy        │  │  ← streaming SSE passthrough
│  └───────────────────────┘  │
└─────────────────────────────┘
  │
  ▼
OpenCode Go upstream
```

## Quick start (local)

```bash
go mod tidy
go run .

# On first run a bootstrap gateway token is printed to stdout, e.g.:
#   sk-...
```

Then open the admin panel at `http://localhost:9812/admin` (default password: `admin`).

Or use the REST API:

```bash
# login
curl -X POST localhost:3000/admin/login -H 'Content-Type: application/json' \
  -d '{"password":"admin"}'   # -> {"token":"eyJ..."}

# add a Go key
curl -X POST localhost:3000/admin/keys \
  -H 'Authorization: Bearer <admin-jwt>' -H 'Content-Type: application/json' \
  -d '{"value":"opencode_xxxx","label":"personal"}'

# list models
curl localhost:3000/v1/models
```

## OpenCode Go models

The default Go catalog includes:

- Chat Completions: `glm-5.1`, `glm-5`, `kimi-k2.7-code`, `kimi-k2.6`, `mimo-v2.5`, `mimo-v2.5-pro`, `deepseek-v4-pro`, `deepseek-v4-flash`
- Messages: `minimax-m3`, `minimax-m2.7`, `minimax-m2.5`, `qwen3.7-max`, `qwen3.7-plus`, `qwen3.6-plus`

At startup the gateway best-effort enriches the in-memory model catalog from
`https://openrouter.ai/api/v1/models`. The matching result adds context length,
architecture metadata, supported parameters, pricing, description, and knowledge
cutoff to `GET /v1/models` and to the admin model table. Internal upstream
model ids and OpenRouter match ids are not shown in the admin catalog view. If
OpenRouter is unavailable, startup continues with the local catalog.

Go usage limits are value-based: 5-hour `$12`, weekly `$30`, and monthly `$60`. Request counts vary by model cost. If limits are reached, the upstream service may fall back to balance usage when enabled in the OpenCode console.

## Cross-protocol conversion

Any client protocol can reach any upstream model. The gateway automatically converts through the IR (Intermediate Representation):

| Client calls           | Model speaks        | What happens                                                  |
| ---------------------- | ------------------- | ------------------------------------------------------------- |
| `/v1/chat/completions` | `messages` (Claude) | Chat → IR → Messages, response Messages → IR → Chat           |
| `/v1/messages`         | `chat` (DeepSeek)   | Messages → IR → Chat, response Chat → IR → Messages           |
| `/v1/responses`        | `messages` (Claude) | Responses → IR → Messages, response Messages → IR → Responses |
| Same protocol          | Same protocol       | Transparent passthrough (no buffering)                        |

Streaming SSE is fully supported in all combinations.

## Model Mapping

Model Mapping lets the gateway rewrite the client-requested `model` before
forwarding to the upstream provider. For example, a client can send
`"model":"gpt-5.5"` while the upstream receives `"model":"glm-51"`.

Configure rules either directly in an environment variable:

```bash
MODEL_MAPPINGS='{"gpt-5.5":"glm-51","gpt-5.5-mini":"glm-5.1"}'
```

or through a JSON file:

```json
{
  "gpt-5.5": "glm-51"
}
```

```bash
MODEL_MAPPING_FILE=./config/model-mapping.example.json
```

When a request body is valid JSON and has a string top-level `model`, the
gateway applies the mapping if present, re-serializes the body, and overwrites
the outbound `Content-Length`. If the body is not valid JSON, has no `model`,
or the model is not mapped, the request is forwarded unchanged with a warning
log for malformed/missing-model bodies. Standard JSON responses and
`stream: true` SSE responses are still proxied transparently.

You can also manage mappings from the admin panel: open `/admin`, then go to
**Model Mappings** to add, edit, or delete client-model → upstream-model rules.
UI-managed rules are persisted in SQLite and take effect immediately.

## Configure opencode to use the gateway

Create `opencode.json` in your project (or `~/.config/opencode/opencode.json`):

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "opencode-sw-chat": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "opencode-sw (Chat)",
      "options": {
        "baseURL": "https://<your-zeabur-domain>/v1",
        "apiKey": "{env:OCSW_TOKEN}"
      },
      "models": {
        "glm-5.1": { "name": "GLM-5.1" },
        "kimi-k2.7-code": { "name": "Kimi K2.7 Code" },
        "deepseek-v4-flash": { "name": "DeepSeek V4 Flash" }
      }
    },
    "opencode-sw-messages": {
      "npm": "@ai-sdk/anthropic",
      "name": "opencode-sw (Messages)",
      "options": {
        "baseURL": "https://<your-zeabur-domain>",
        "apiKey": "{env:OCSW_TOKEN}",
        "headers": { "anthropic-version": "2023-06-01" }
      },
      "models": {
        "minimax-m3": { "name": "MiniMax M3" },
        "qwen3.7-plus": { "name": "Qwen3.7 Plus" }
      }
    }
  },
  "model": "opencode-sw-chat/glm-5.1"
}
```

```bash
export OCSW_TOKEN=sk-...
```

> Note: for the Anthropic provider, point `baseURL` at the gateway root (no `/v1`); the SDK appends `/v1/messages` itself. For the OpenAI-compatible provider, include `/v1`.

## Admin panel

Access at `http://<gateway>/admin` (default password: `admin`). Features:

- **Dashboard** — total calls, key/token counts, avg latency, calls-by-model chart, calls-by-protocol chart, recent call log
- **API Keys** — add/remove/toggle keys, edit key value/label/weight/proxy settings, reset cooldown, view fail counts and usage
- **Tokens** — create/delete/copy `sk-` gateway tokens with optional rate limits
- **Models** — manage the Go model routing table and view OpenRouter-enriched context length, input/output/cache pricing, and capability tags
- **Model Mappings** — manage client model → upstream model rewrite rules, persisted in SQLite and applied immediately

### Admin Panel UI

- 🌓 **Dark/Light theme** — toggle via the top bar dropdown, preference saved to localStorage
- 🌐 **Language switch** — Chinese/English dropdown in the top bar
- 📋 **Custom confirm dialogs** — replaces native `confirm()` for all delete operations and logout
- 🔽 **Dropdown menus** — language and theme selectors with smooth animation
- ⚡ **Modular architecture** — Vue 3 SPA with ES modules (no build step required)

## Deploy on Zeabur

1. Push this repo to GitHub and import it in Zeabur (auto-detected via `Dockerfile`/`zeabur.json`).
2. Under **Volumes**, add a persistent volume mounted at `/data`.
3. Set environment variables:
   - `ADMIN_PASSWORD` — admin panel password
   - `JWT_SECRET` — random string for JWT signing
   - `GO_BASE_URL` — override the Go upstream if needed
4. Deploy. Zeabur assigns a domain; health check hits `/health`.

## Environment variables

| Var                | Default                      | Description                                              |
| ------------------ | ---------------------------- | -------------------------------------------------------- |
| `PORT`             | `9812`                       | HTTP listen port (use env to override, e.g. `PORT=3000`) |
| `ADMIN_PASSWORD`   | `admin`                      | Admin login password                                     |
| `JWT_SECRET`       | (built-in)                   | Secret for admin JWT                                     |
| `DB_PATH`          | `./data/opencode-sw.db`      | SQLite file path                                         |
| `GO_BASE_URL`      | `https://opencode.ai/zen/go` | Go upstream base                                         |
| `MODEL_MAPPINGS`  | empty                        | Optional JSON object mapping requested model → upstream model |
| `MODEL_MAPPING_FILE` | empty                     | Optional JSON file path for model mappings               |
| `GROUP_MULTIPLIERS` | empty                      | Optional group billing multipliers, e.g. `{"go":0.8}` or `go=0.8,default=1` |
| `UPSTREAM_TIMEOUT` | `120`                        | Upstream call timeout (seconds)                          |

## Project structure

```
├── main.go              # Entry point
├── config/              # Env-based config + model routing table
│   ├── config.go
│   └── models.go
├── store/               # SQLite models (Key, Token, UsageLog)
│   └── sqlite.go
├── pool/                # Key pool (weighted picker, cooldown) + token mgmt
│   ├── key.go
│   └── token.go
├── protocol/            # IR + encode/decode for all 3 protocols + streaming
│   ├── types.go         # IR definitions
│   ├── chat.go          # OpenAI Chat Completions ↔ IR
│   ├── messages.go      # Anthropic Messages ↔ IR
│   ├── responses.go     # OpenAI Responses ↔ IR
│   ├── stream_chat.go   # Chat SSE decoder/encoder
│   ├── stream_messages.go   # Anthropic SSE decoder/encoder
│   ├── stream_responses.go  # Responses SSE decoder/encoder
│   ├── convert.go       # Universal cross-protocol converter
│   └── convert_test.go  # Tests
├── api/                 # Public API (proxy + auth + rate limit)
│   ├── router.go
│   ├── proxy.go         # Cross-protocol proxy handler
│   ├── middleware.go     # Token auth
│   ├── models.go        # GET /v1/models
│   └── ratelimit.go     # Per-token rate limiter
├── admin/               # Admin REST API
│   └── router.go
├── web/                 # Embedded admin SPA (modular ES modules)
│   ├── embed.go         # embed.FS multi-file server
│   ├── admin.html       # Vue template (slim, ~490 lines)
│   ├── css/
│   │   └── admin.css    # Styles with dark/light theme variables
│   └── js/
│       ├── app.js       # Vue 3 app entry
│       ├── icons.js     # SVG icons
│       ├── locales.js   # i18n (zh/en)
│       ├── api.js       # API client
│       └── pages/       # Page composables
│           ├── dashboard.js
│           ├── keys.js
│           ├── tokens.js
│           └── models.js
├── upstream/            # HTTP client for upstream calls
│   └── client.go
├── Dockerfile
├── zeabur.json
└── PLAN.md
```

## License

MIT
