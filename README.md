# Nexa Gateway

Nexa Gateway is a self-hosted LLM gateway written in Go. It gives OpenAI-compatible clients one endpoint for OpenAI, Groq, Gemini, Anthropic, and custom OpenAI-compatible providers while recording every request in a built-in operations dashboard.

The dashboard includes smart routing profiles, a chat-capability-filtered playground, readable and raw-JSON trace inspection, interactive traffic ranges from five minutes to 30 days (plus custom dates), and separate upstream-provider versus Nexa gateway latency metrics.

## Run with Docker

```bash
docker build -t nexa-gateway .
docker run --name nexa-gateway -p 8080:8080 -v nexa-data:/data nexa-gateway
```

On the first start, the container prints a one-time master key beginning with `nexa_`. Save it, then open <http://localhost:8080> and sign in. Provider credentials and gateway data persist in the `nexa-data` volume.

You can also use Compose:

```bash
docker compose up --build
```

If port 8080 is already occupied, choose another host port without changing the container:

```bash
NEXA_PORT=18080 docker compose up --build
```

For HTTPS deployments, terminate TLS in a reverse proxy and set `NEXA_SECURE_COOKIES=true`.

## Connect an application

Add a provider in the dashboard (its key is verified on save), create a per-app API key under **Access → API keys**, and use `slug/model` as the model name:

```python
from openai import OpenAI

client = OpenAI(
    api_key="nexa_sk_YOUR_APP_KEY",  # or the master key
    base_url="http://localhost:8080/v1",
)

response = client.chat.completions.create(
    model="groq/openai/gpt-oss-20b",
    messages=[{"role": "user", "content": "Hello from Nexa"}],
)
print(response.choices[0].message.content)
```

With exactly one enabled provider, bare model names also work — including ids that contain a slash, such as `openai/gpt-oss-20b` on Groq. You can alternatively select a provider using the `X-Nexa-Provider` header. Every response carries an `X-Nexa-Trace-Id` header that identifies the matching trace.

## Configuration

| Environment variable | Default | Purpose |
|---|---:|---|
| `NEXA_ADDR` | `:8080` | HTTP listen address |
| `NEXA_DATA` | `./data` | SQLite database and encryption-key directory |
| `NEXA_SECURE_COOKIES` | `false` | Require HTTPS for dashboard session cookies |
| `JEV_API_KEY` | empty | Optional TypeSafe Jev key for Jev-based routing profiles |
| `TYPESAFE_API_KEY` | empty | Official TypeSafe key name; used when `JEV_API_KEY` is empty |
| `NEXA_TRACE_RETENTION_DAYS` | `90` | Delete traces older than this many days (`0` keeps them forever) |

The provider encryption key is generated at `NEXA_DATA/secret.key`; back it up together with `nexa.db`. Losing it makes saved provider credentials unrecoverable.

If the master key is lost, rotate it using the same persistent volume:

```bash
docker compose run --rm nexa-gateway -reset-master
```

This prints the replacement once, immediately invalidates the previous master key, and signs out every master session.

## Supported API

- `POST /v1/chat/completions` — OpenAI-compatible chat completions, including streaming (authenticate with the master key or a per-app API key)
- `GET /v1/models` — model catalog aggregated in parallel from enabled providers and cached for five minutes
- `GET /healthz` — container health
- Dashboard APIs under `/api/*` use an HTTP-only session cookie

OpenAI, Groq, Gemini, and custom compatible routes pass the OpenAI request through; direct routes retry one `429`/`5xx`/network failure, honouring `Retry-After`, and forward `Retry-After` and `x-ratelimit-*` headers. Streamed calls record token usage even when the client did not request it (Nexa asks OpenAI/Groq for usage and hides that extra chunk). Anthropic is translated in both directions: system prompts, images, tool definitions, `tool_choice`, multi-turn tool calls and results, stop sequences, streamed text and streamed tool calls, cache-read tokens, and errors in OpenAI shape. Custom providers can send extra headers on every request.

Traces capture every call — including rejected ones — with the current turn, the full request, parameters, response and tool calls, finish reason, input/output/cached/reasoning tokens, time to first token, provider versus Nexa latency, cost, caller (API key or dashboard user) and request id. Cost uses a built-in price table plus prices you add under **Providers → Model pricing** (`model*` matches a prefix).

## Smart routing

Create a profile in **Smart Routing**, add model targets from any configured provider, and activate one profile as the default. Applications can then use `smart` as the model name. A specific profile is addressable as `smart/profile-slug` even when it is not the active default.

Prompt difficulty is decided by **Jev by TypeSafe**, which reads the system prompt and the latest turns and returns a difficulty with a confidence score. Decisions are cached for ten minutes. Supply `JEV_API_KEY` or enter a key in the profile editor (AES-256-GCM encrypted); **TEST JEV** on a profile card makes a live check. If Jev is unavailable, the profile's chosen fallback lane (Medium by default) is used and the reason is recorded in the trace. When Jev's confidence is below the profile threshold, Nexa moves one lane up.

Smart routing selects between Light, Medium, and Heavy model lanes. Lanes can declare that a model lacks tools, vision, or JSON mode, and requests needing those skip them; among equally matched lanes a healthy route is preferred. Retryable failures (`408`, `409`, `425`, `429`, `5xx`, and first-byte timeouts) are retried and then move down the fallback ladder; `401`/`403`/`404` skip to the next lane; any other `4xx` stops immediately because every lane would reject the same request. A client disconnect stops routing without counting against provider health.

An attempt stays private until its provider answers `2xx`; from then on the response streams straight to the client, so smart routes keep both failover and real streaming. The per-profile timeout only covers the wait for a provider to start answering — a long generation already in progress is never cut off. Responses include `X-Nexa-Routed-Model`, `X-Nexa-Routing-Lane`, and `X-Nexa-Routing-Confidence`. The **Try a prompt** panel previews the decision for any prompt.

## Security model

- The master key is stored as bcrypt and SHA-256 digests; the 192-bit random key is checked with a constant-time SHA-256 compare so API calls do not pay bcrypt's cost. Its plaintext is shown only on creation or rotation.
- Per-app API keys (`nexa_sk_…`) are stored only as SHA-256 digests, shown once, named in traces, and revocable.
- Provider API keys and extra headers are encrypted at rest with AES-256-GCM.
- Roles are enforced server-side: the master owner can do everything; admins manage providers, routing, pricing, users and API keys; members can view and use the playground. Identity comes only from the session, never from request headers.
- Dashboard sessions are HTTP-only, SameSite Strict, stored as SHA-256 digests, expire after seven days, and enforce same-origin writes. Changing or resetting a password signs out the user's other sessions.
- Ten failed sign-ins from one address lock sign-in for ten minutes; unknown usernames take as long to reject as wrong passwords.
- Secrets are never returned by provider-list APIs or written to request logs.

All application code and embedded assets live in this `GATEWAY` directory. Black-box test tooling is intentionally isolated in the sibling `TESTING` directory.
