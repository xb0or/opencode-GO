# opencode-sw

A multi-key management gateway for **OpenCode Zen / Go** services, written in Go + Gin and ready to deploy on **Zeabur**.

It aggregates many OpenCode API keys (Zen pay-as-you-go _and_ Go subscription) behind a single set of universal endpoints, so any OpenAI- / Anthropic-compatible client (including opencode itself) can consume them with one gateway token.

## Features

- 🔑 **Multi-key pool** with weighted round-robin scheduling, exponential-backoff failure cooldown, and usage counting
- 🔄 **Full cross-protocol conversion** — call any model from any endpoint:
  - Use `/v1/chat/completions` to talk to Claude (Messages protocol)
  - Use `/v1/messages` to talk to GPT-5 (Responses protocol)
  - Use `/v1/responses` to talk to DeepSeek (Chat protocol)
  - All conversions work with **streaming SSE** too
- 🧭 **Model routing table** mapping gateway model ids → real upstream model + protocol
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
OpenCode Zen / Go upstream
```

## Quick start (local)

```bash
go mod tidy
go run .

# On first run a bootstrap gateway token is printed to stdout, e.g.:
#   ocsw-...
```

Then open the admin panel at `http://localhost:9812/admin` (default password: `admin`).

Or use the REST API:

```bash
# login
curl -X POST localhost:3000/admin/login -H 'Content-Type: application/json' \
  -d '{"password":"admin"}'   # -> {"token":"eyJ..."}

# add a Zen key
curl -X POST localhost:3000/admin/keys \
  -H 'Authorization: Bearer <admin-jwt>' -H 'Content-Type: application/json' \
  -d '{"value":"opencode_xxxx","group":"zen","label":"personal"}'

# list models
curl localhost:3000/v1/models
```

## Cross-protocol conversion

Any client protocol can reach any upstream model. The gateway automatically converts through the IR (Intermediate Representation):

| Client calls           | Model speaks        | What happens                                                  |
| ---------------------- | ------------------- | ------------------------------------------------------------- |
| `/v1/chat/completions` | `messages` (Claude) | Chat → IR → Messages, response Messages → IR → Chat           |
| `/v1/messages`         | `chat` (DeepSeek)   | Messages → IR → Chat, response Chat → IR → Messages           |
| `/v1/responses`        | `messages` (Claude) | Responses → IR → Messages, response Messages → IR → Responses |
| Same protocol          | Same protocol       | Transparent passthrough (no buffering)                        |

Streaming SSE is fully supported in all combinations.

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
        "glm-4.6": { "name": "GLM 4.6" },
        "deepseek-v3.2": { "name": "DeepSeek V3.2" },
        "kimi-k2": { "name": "Kimi K2" }
      }
    },
    "opencode-sw-messages": {
      "npm": "@ai-sdk/anthropic",
      "name": "opencode-sw (Claude)",
      "options": {
        "baseURL": "https://<your-zeabur-domain>",
        "apiKey": "{env:OCSW_TOKEN}",
        "headers": { "anthropic-version": "2023-06-01" }
      },
      "models": {
        "claude-sonnet-4.5": { "name": "Claude Sonnet 4.5" }
      }
    }
  },
  "model": "opencode-sw-chat/glm-4.6"
}
```

```bash
export OCSW_TOKEN=ocsw-...
```

> Note: for the Anthropic provider, point `baseURL` at the gateway root (no `/v1`); the SDK appends `/v1/messages` itself. For the OpenAI-compatible provider, include `/v1`.

## Admin panel

Access at `http://<gateway>/admin` (default password: `admin`). Features:

- **Dashboard** — total calls, key/token counts, avg latency, calls-by-model chart, calls-by-protocol chart, recent call log
- **API Keys** — add/remove/toggle keys, set proxy URL, reset cooldown, view fail counts and usage
- **Tokens** — create/delete gateway tokens with optional group restrictions (checkbox multi-select) and rate limits
- **Models** — manage the model routing table (add custom models, change upstream/protocol, select real model from dropdown)

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
   - `ZEN_BASE_URL` / `GO_BASE_URL` — override upstreams if needed
4. Deploy. Zeabur assigns a domain; health check hits `/health`.

## Environment variables

| Var                | Default                      | Description                                              |
| ------------------ | ---------------------------- | -------------------------------------------------------- |
| `PORT`             | `9812`                       | HTTP listen port (use env to override, e.g. `PORT=3000`) |
| `ADMIN_PASSWORD`   | `admin`                      | Admin login password                                     |
| `JWT_SECRET`       | (built-in)                   | Secret for admin JWT                                     |
| `DB_PATH`          | `./data/opencode-sw.db`      | SQLite file path                                         |
| `ZEN_BASE_URL`     | `https://opencode.ai/zen`    | Zen upstream base                                        |
| `GO_BASE_URL`      | `https://opencode.ai/zen/go` | Go upstream base                                         |
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
