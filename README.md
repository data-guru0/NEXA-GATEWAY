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

Add a provider in the dashboard, note its routing slug, and use `slug/model` as the model name:

```python
from openai import OpenAI

client = OpenAI(
    api_key="nexa_YOUR_MASTER_KEY",
    base_url="http://localhost:8080/v1",
)

response = client.chat.completions.create(
    model="groq/llama-3.3-70b-versatile",
    messages=[{"role": "user", "content": "Hello from Nexa"}],
)
print(response.choices[0].message.content)
```

With exactly one enabled provider, bare model names also work. You can alternatively select a provider using the `X-Nexa-Provider` header.

## Configuration

| Environment variable | Default | Purpose |
|---|---:|---|
| `NEXA_ADDR` | `:8080` | HTTP listen address |
| `NEXA_DATA` | `./data` | SQLite database and encryption-key directory |
| `NEXA_SECURE_COOKIES` | `false` | Require HTTPS for dashboard session cookies |
| `JEV_API_KEY` | empty | Optional TypeSafe Jev key for Jev-based routing profiles |
| `TYPESAFE_API_KEY` | empty | Official TypeSafe key name; used when `JEV_API_KEY` is empty |

The provider encryption key is generated at `NEXA_DATA/secret.key`; back it up together with `nexa.db`. Losing it makes saved provider credentials unrecoverable.

If the master key is lost, rotate it using the same persistent volume:

```bash
docker compose run --rm nexa-gateway -reset-master
```

This prints the replacement once and immediately invalidates the previous master key.

## Supported API

- `POST /v1/chat/completions` — OpenAI-compatible chat completions, including streaming
- `GET /v1/models` — live model catalog aggregated from enabled providers
- `GET /healthz` — container health
- Dashboard APIs under `/api/*` use an HTTP-only session cookie

OpenAI, Groq, Gemini, and custom compatible routes pass the OpenAI request through. Anthropic messages are translated in both directions; text streaming and standard function tools are supported.

## Smart routing

Create a profile in **Smart Routing**, add model targets from any configured provider, and activate one profile as the default. Applications can then use `smart` as the model name. A specific profile is addressable as `smart/profile-slug` even when it is not the active default.

Prompt difficulty is decided by **Jev by TypeSafe**, which returns a semantic choice and a confidence score through the System One API. Supply `JEV_API_KEY` or enter a key in the profile editor; entered keys are AES-256-GCM encrypted. If no key is configured or Jev is unreachable, the request is routed to the Heavy lane and the reason is recorded in the trace.

Smart routing selects between Light, Medium, and Heavy model lanes from prompt difficulty. Requests retry retryable failures (`408`, `409`, `425`, `429`, and `5xx`) before moving through the real fallback ladder. A route advances to its configured fallback only after a real request failure; Jev confidence and route-health history do not silently move a request to a different difficulty lane.

Smart routes buffer streaming responses until an upstream attempt succeeds so that failover remains possible. Direct `provider/model` routes retain their existing streaming behavior.

## Security model

- The master key is bcrypt-hashed; its plaintext is shown only on creation or rotation.
- Provider API keys are encrypted at rest with AES-256-GCM.
- Dashboard sessions are HTTP-only, SameSite Strict, expire after seven days, and enforce same-origin writes.
- Secrets are never returned by provider-list APIs or written to request logs.

All application code and embedded assets live in this `GATEWAY` directory. Black-box test tooling is intentionally isolated in the sibling `TESTING` directory.
