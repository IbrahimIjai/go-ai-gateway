# MiniRouter — Go LLM gateway (runnable scaffold)

**Choose your path:**

- **Run it now:** follow [Quick start](#quick-start-go-123) below; no vendor credentials required.
- **Work on it:** [Development commands](#development-commands) for run, build, test, vet and format.
- **Configure models and providers:** [example catalog](config/minirouter.example.json), explained in [Catalog file](#catalog-file).
- **Inspect the future contract/schema:** [OpenAPI design](contracts/openapi.yaml) and [PostgreSQL migrations](db/migrations/).

**Important:** `db/` and `contracts/` are **design artifacts**, not currently connected to the running app.

**Runnable starter / learning project.** An OpenAI-compatible Chat Completions API (text and tool calling) backed by a real provider abstraction, normal and SSE-streaming adapters for OpenAI, Anthropic and Gemini, a basic fallback router and an in-memory usage journal.

**No account, payment, or real API key is required to run the included `demo/echo` model.** The demo is deterministic and is **not an AI model**.

> **Scope warning:** This is a development scaffold, not a production billing platform or a complete OpenAI API implementation. It does not charge customers or reserve their credits. Vendor integrations have mock-upstream tests but have **not** been live-tested with paid credentials in this package. Provider model IDs and rates must be rechecked for your account. See [Known limits](#known-limits).

## Quick start (Go 1.23+)

```bash
# From the repository root.
# Use a long random key for real deployments; localhost demo only here.
export MINIROUTER_API_KEY=local-demo-secret-123

go test ./...
go run ./cmd/minirouter
```

Open another terminal:

```bash
curl http://127.0.0.1:8080/healthz

curl -s http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer local-demo-secret-123'

curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer local-demo-secret-123' \
  -H 'Content-Type: application/json' \
  -d @examples/chat.json
```

Streaming:

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer local-demo-secret-123' \
  -H 'Content-Type: application/json' \
  -d @examples/stream.json
```

Check usage (stored only for this server process):

```bash
curl -s http://127.0.0.1:8080/v1/usage \
  -H 'Authorization: Bearer local-demo-secret-123'
```

### Add real providers

Export one or more credentials before starting the server:

```bash
export MINIROUTER_API_KEY=local-demo-secret-123
export OPENAI_API_KEY='...'
export ANTHROPIC_API_KEY='...'
export GEMINI_API_KEY='...'

go run ./cmd/minirouter
```

**Only configured providers appear in `/v1/models`.** The following are initial *example* default IDs, not guaranteed available in your account:

- `openai/gpt-4.1-mini` → OpenAI `/v1/chat/completions`
- `anthropic/claude-haiku-4-5` → Anthropic `/v1/messages`
- `google/gemini-2.5-flash` → Gemini `generateContent`
- `demo/echo` → deterministic offline test adapter
- `router/auto` → configured live models in order; `router/eco` cheapest and `router/fast` fastest first; demo if no live providers

If an ID has changed or isn't available, override it with `OPENAI_MODEL`, `ANTHROPIC_MODEL`, or `GEMINI_MODEL`. See `.env.example`.

Try an explicit live model by replacing `demo/echo` in `examples/chat.json` with an ID from `/v1/models`.

> **Costs:** Real vendor calls can cost money. In this starter, your provider account pays those costs. The project will **not** enforce spending limits. Start with small token limits and provider-side budget controls.

### Optional: configure pricing (not automatic)

The gateway can demonstrate metering costs, but it intentionally does **not** hardcode real pricing or automatically fetch it. Prices vary by model, vendor, cached-token category, region/tier and time. You must set both input and output USD-per-million-token rates for each provider model if you want estimates:

```bash
# Example of config syntax ONLY — these rates are invented, NOT current market rates.
export OPENAI_INPUT_USD_PER_1M='1.250000'
export OPENAI_OUTPUT_USD_PER_1M='5.000000'
export OPENAI_CACHE_READ_USD_PER_1M='0.500000'
```

The billing package calculates to **microUSD** using integer arithmetic. Known-cost requests appear in `/v1/usage` as `cost_usd`, while requests with missing prices have no cost value and are counted as `unpriced_requests`.

## Catalog file

Environment variables are enough for a quick start. For more than one model per provider, OpenAI-compatible hosts (Moonshot, Together, Fireworks, ...) or one model served by several hosts, describe the catalog in JSON and point `MINIROUTER_CONFIG` at it:

```bash
export MINIROUTER_CONFIG=config/minirouter.example.json
go run ./cmd/minirouter
```

- **models** are what callers ask for, with capabilities and limits.
- **deployments** are where a model runs: provider, upstream model ID, priority, weight, concurrency cap and price. An exact model ID only ever uses its own deployments.
- **aliases** such as `router/auto` try several models: `ordered` as listed, `eco` by bounded cost (unpriced models excluded), `fast` by observed latency, `auto` by a weighted score. Models that cannot honor a request (tools, JSON output, context size) are excluded first, and a request no model can serve gets a 400 saying why. `POST /v1/route/explain` shows the plan without calling anyone.
- **providers** name the environment variable holding each key; secrets never go in the file.

Startup validation reports every problem at once, and unknown fields are errors. Prices are USD per million tokens as exact decimal strings; a price is billed only once `verified_at` is set. To draft entries from LiteLLM's price list (MIT):

```bash
go run ./cmd/catalog-import -prices model_prices_and_context_window.json gpt-4.1-mini claude-haiku-4-5
```

## Development commands

Run these from the repository root. The Go toolchain is the build system; there is no Makefile.

```bash
go run ./cmd/minirouter                      # run (needs MINIROUTER_API_KEY, see Quick start)
go build -o bin/minirouter ./cmd/minirouter  # build; creates bin/, which git ignores
./bin/minirouter                             # run the built binary

go test ./...                                # tests: unit + local mock upstreams, no external network
go test -race ./...                          # tests with the race detector
go vet ./...                                 # static checks
gofmt -l cmd internal                        # list unformatted files; no output means clean
gofmt -w cmd internal                        # format in place
```

Before committing, `gofmt -l cmd internal` should print nothing and `go vet ./...` and `go test -race ./...` should pass.

## What it implements

| Feature | Status |
|---|---|
| `POST /v1/chat/completions`: text, content parts, tools | ✅ implemented |
| `GET /v1/models` | ✅ implemented |
| `GET /v1/usage` (demo memory) | ✅ implemented |
| API bearer key | ✅ simple single local key |
| OpenAI, Anthropic, Gemini calls | ✅ adapter code + mocked tests |
| Provider-specific translation of messages, tools and tool results | ✅ OpenAI, Anthropic, Gemini |
| SSE streaming normalized to Chat Completions chunks | ✅ text and tool-call deltas |
| Usage metadata normalized and recorded | ✅ when upstream returns usage |
| `router/auto` fallback before the first byte, on provider-side failures | ✅ |
| Deployment priority tiers and weights, retries with backoff, Retry-After within a budget | ✅ |
| Per-deployment circuit breaker, concurrency limit, timed auto-disable on quota/key failures | ✅ |
| Routing explained: `X-MiniRouter-Route-Reason`, `-Attempts`, `-Deployment` headers, journaled per request | ✅ |
| Cut or silent upstream streams end with an error, never a false `[DONE]` | ✅ |
| Policy aliases: `router/eco` (cheapest bounded cost), `router/fast` (lowest latency), `router/auto` (weighted quality, cost, speed, reliability) | ✅ |
| Capability gates that refuse rather than guess; `POST /v1/route/explain` dry run | ✅ |
| `response_format` JSON / JSON schema | ✅ OpenAI, Gemini (Anthropic: refused, never dropped) |
| Images / audio | ☐ future |
| Catalog: models vs deployments, JSON config, OpenAI-compatible hosts | ✅ |
| Persistent DB ledger / balances / multi-tenant keys | ☐ future |
| Credit reservation/settlement/reconciliation | ☐ future |
| MCP / x402 / agent payment | ☐ optional future |

## Internal structure

```text
cmd/minirouter/main.go       configuration and server boot
cmd/catalog-import/           draft catalog entries from LiteLLM prices
internal/api/                 HTTP handlers, strict input validation, SSE output
internal/core/                shared request, message, tool, usage and price types
internal/catalog/             models, deployments, aliases; JSON config and validation
internal/provider/            upstream adapters and SSE parsing
internal/router/              resolution, attempt planning, retries, stream preflight
internal/health/              circuit breakers, auto-disable, concurrency limits, latency samples
internal/billing/             microUSD cost calculation, demo usage journal
examples/                     request JSON
config/                      example catalog
contracts/                   target OpenAPI (not wired in)
db/migrations/               planned PostgreSQL tables (not applied)
```

Request path:

```text
client -> API auth/validate -> router/model registry -> provider adapter
                                                -> provider HTTP API
client <- normalized JSON or SSE <- usage metadata <- adapter
                              -> usage journal -> cost estimate if configured
```

## Known limits

1. **Text and tools only**: images, audio, `n`, logprobs and other unsupported fields are rejected rather than silently dropped. A feature one provider cannot honor (strict tool schemas outside OpenAI, `response_format` on Anthropic) is refused with a 400 for that model; `router/auto` moves on to a model that can. No Responses API support yet.
2. **This is not an accounting ledger.** All usage is in-process and disappears on restart. No pre-authorization, charging, invoices, refunds or tenant isolation.
3. **Real pricing is optional and must be verified.** Missing pricing is unknown, not free. Cache billing variants and Gemini thought tokens may need vendor/tier-specific treatment before real financial use.
4. **Streaming:** upstream errors after output cannot be safely retried. A stream counts as finished only on the provider's end marker; a cut connection, or 120 s without upstream data, ends it with an error frame and no `[DONE]`. Interrupted streams are logged as `usage_unavailable`; this requires later reconciliation. Multimodal stream events are not supported.
5. **Fallback policy:** `router/auto` moves to the next model only before the first byte reaches the client, and only for provider-side failures: rate limits, timeouts, 5xx, network errors, an exhausted provider quota or a refused provider key. An invalid request is never retried elsewhere. Set `MINIROUTER_FIRST_EVENT_TIMEOUT` (for example `20s`) to also hand over when a model is slow to start; the last candidate always waits. It does not provide cross-provider fallback for an explicit model name. `router/auto` may change the chosen model and must not be used when a caller requires one exact model.
6. **Security:** no HTTPS server, audit logging, abuse prevention, per-client rates or durable key storage. Local binding defaults to `127.0.0.1` for safety. Put a trusted TLS-terminating reverse proxy in front if deploying.
7. **No concurrency-based money enforcement** yet; `/v1/usage` is observational. Configure upstream hard limits yourself.
8. **The toy `demo/echo` counts are invented** and are only for demo. All real prices and token counts should use authentic provider usage and reconciled billing.

## Source references

- [OpenAI Chat Completions](https://platform.openai.com/docs/api-reference/chat/create)
- [OpenAI streaming guide](https://platform.openai.com/docs/guides/streaming-responses)
- [Anthropic Messages API](https://docs.anthropic.com/en/api/messages)
- [Anthropic streaming messages](https://docs.anthropic.com/en/docs/build-with-claude/streaming)
- [Gemini `generateContent` / `streamGenerateContent`](https://ai.google.dev/api/generate-content)
- [Bifrost — Go LLM gateway](https://github.com/maximhq/bifrost)
- [New API — Go billing/gateway reference](https://github.com/QuantumNous/new-api)
- [LiteLLM — model/provider registry ideas](https://github.com/BerriAI/litellm)
- [ClawRouter — smart model routing](https://github.com/BlockRunAI/ClawRouter)

## License

MIT, see `LICENSE`. This is an independent educational starter, not an OpenRouter, OpenAI, Anthropic or Google product.
