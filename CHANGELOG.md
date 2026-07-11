# Changelog

## [Unreleased]

### Features

- Force 1M context window for capable Claude models (opus/sonnet 4.6+): always inject the `anthropic-beta: context-1m-2025-08-07` header upstream so requests no longer fall back to the 200k hard limit. Replaces the old `-1m` model-suffix swap (those variants no longer exist in Copilot's model list). Applies to native `/v1/messages`, `/chat/completions`, and `/responses` routes; merges with any client-provided beta header.
- Add debug request capture: set `COPILOT2API_DEBUG_MODELS=gpt-5.4,gpt-5.5` to save request bodies as formatted JSON under `{dataDir}/debug/{model}/`. Captures both OpenAI and Anthropic API requests. One file per request, named `{datetime}-{rand4hex}.json`
- Add usage statistics system: records token usage per request to JSONL files (`stats/` package)
- Add control plane endpoints: `GET /usage` (query aggregated stats), `GET /usage/accounts` (list accounts with data), `GET /dashboard` (HTML dashboard), `GET /usage/pricing` (LiteLLM pricing data)
- Add interactive HTML dashboard with Chart.js stacked bar charts, time range selectors, cache hit rate cards, estimated cost, and auto-refresh
- Stats opt-in via `COPILOT2API_STATS_ENABLED=true` env var (default: disabled). When disabled, stats recording, dashboard, and usage endpoints are inactive
- Stats directory configurable via `COPILOT2API_STATS_DIR` env var (default: `~/.config/copilot2api/stats`)
- Dynamic model pricing: fetches LiteLLM pricing JSON on startup and daily at 3:00 AM UTC, cached to disk. Supports fuzzy model name matching (provider prefixes, version normalization, suffix stripping)
- Dashboard: multi-select dimensions (By Model + By Account), month/quarter range pickers, manual refresh button, configurable auto-refresh (off/1m/2m/5m/30m/1h) stored in localStorage
- Dashboard: API key required for usage data (stored in localStorage), bar top labels with overlap avoidance, bottom dimension labels at 60°
- Dashboard: show the running binary's short git commit next to the page title

### Docs

- Add `requires_openai_auth = false` to Codex config example in README

### Bug Fixes

- Serve the dashboard's Chart.js dependency from the embedded control server assets so offline or restricted workstations do not need CDN access

## [0.3.1] - 2026-04-26

### Bug Fixes

- Fix Anthropic thinking signatures being emitted as a separate block instead of attached to the currently open thinking block
- Fix Docker image crash (`exec /copilot2api: no such file or directory`) caused by dynamically-linked binary in `scratch` image — add `CGO_ENABLED=0` to CI cross-compilation
- Fix Docker multi-arch build: arm64 image was shipping the amd64 binary due to `ARG TARGETARCH=amd64` default overriding buildx's automatic platform arg
- Fix CI triggering redundant runs on tag pushes — `on: push` now scoped to `main` branch only

### CI

- Add Docker smoke test — `docker run --version` gate before pushing to prevent broken images from reaching the registry

### Docs

- Refresh README quick start and examples

## [0.3.0] - 2026-04-03

### Features

- Add Gemini-compatible `/v1beta/models` endpoints for local `gemini-cli` usage, including `generateContent`, `streamGenerateContent`, and `countTokens`
- Expose the full upstream model list on the Gemini `/v1beta/models` surface instead of limiting the listing to a small allowlist
- Add smart fallback routing between `/v1/chat/completions` and `/v1/responses`, so requests can still work when a model only supports one of the two OpenAI-compatible endpoints
- Improve OpenAI request conversion compatibility across the two endpoints, including better handling for system instructions, structured output, tool choice, reasoning state, and `previous_response_id`
- Improve Claude Code native `/v1/messages` compatibility by removing unsupported passthrough fields before forwarding requests upstream
- Add AmpCode support: chat completions via `/amp/v1/*` and `/api/provider/*` route through Copilot API; management routes (`/api/*`) and login redirects reverse-proxy to `ampcode.com`

## [0.2.0]

### Performance

- Batch SSE flushes in Anthropic streaming — flush once per upstream event instead of per translated event (~3-5x fewer syscalls)
- Flush at SSE event boundaries in native `/v1/messages` passthrough instead of every line (~3x fewer syscalls)
- Defer model alias body re-encode to only the native passthrough path — Responses and Chat Completions paths skip the JSON round-trip entirely
- Remove unnecessary `string()` copy in `writeSSEEvent`

### Architecture

- Consolidate models cache — single upstream `/models` fetch populates both raw JSON (for proxying) and parsed model info (for capability detection), eliminating duplicate HTTP calls
- Remove dead `internal/cache` package after consolidation
- Centralize request body size limit as `upstream.MaxRequestBody` constant (was magic number `10<<20` in 3 files)
- Consistent SSE header setup via `sse.BeginSSE()` across all streaming paths

### Logging

- nginx-style single access log per request at completion with method, endpoint, model, route, duration
- Downgrade client disconnect / context cancellation errors from ERROR to WARN via `upstream.LogRequestError`
- Add `duration_ms` to token refresh logs
- Promote key request lifecycle logs to Info level (was all Debug — invisible in default mode)
- Remove noisy per-chunk/per-event debug logs from streaming hot path
- Add `route` field to Anthropic access log (`native`, `responses`, `chat_completions`)
- Add `endpoint` field to Anthropic access log for consistency with proxy handler
- Add models cache miss debug log

### Bug Fixes

- Fix split choices in OpenAI Chat Completions responses — merge text and tool_calls from separate choices into a single Anthropic message
- Fix `AnthropicContentBlockDelta` / `AnthropicMessageDelta` type confusion in streaming events
- Remove hardcoded "Thinking..." placeholder text in thinking blocks
- Request usage in streaming chunks (`stream_options.include_usage`) so `message_delta` gets real output token counts

### Features

- 1M context window support — automatically appends `-1m` suffix when `anthropic-beta: context-1m-...` header is detected
- Document 1M context window usage in README

## [0.1.0]

- Initial commit
