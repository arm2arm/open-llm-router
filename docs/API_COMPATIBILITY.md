# API compatibility

OLR implements a subset of the OpenAI and Anthropic HTTP APIs for native inference
backends. It does not implement either provider's entire platform API or translate
one protocol into the other. This inventory was checked against the route registry
and the official API references on 2026-11-16.

## OpenAI

| Endpoint or feature | OLR coverage |
|---|---|
| `POST /v1/chat/completions` | Native JSON and SSE; tools, tool results and provider fields pass through. |
| `POST /v1/completions` | Native JSON and SSE. |
| `POST /v1/embeddings` | Native JSON. An embedding-capable backend/model is required. |
| `GET /v1/models` | OLR's access-filtered models and aliases in the OpenAI list format, rather than the backend catalog. |
| `POST /v1/responses` | Native stateless JSON and SSE, including full-history tool loops, structured output and reasoning fields. Requires a backend implementing Responses. |
| Stored Chat Completions retrieval/list/update/delete and message listing | Not implemented. |
| Responses retrieval/delete/cancel/input-item listing, compact and input-token counting | Not implemented. |
| Responses storage, background mode, `previous_response_id`, Conversations and stored input references | Refused by the create route; OLR requires full input history and uses `store:false`. |
| Responses WebSockets, Realtime/Live transports | Not implemented. |
| Audio, image endpoints, moderations | Not implemented. Multimodal fields inside a supported inference request still pass through to its backend. |
| Files/uploads, batches, fine-tuning, vector stores, containers, evals, agents, webhooks and provider administration | Not implemented. |

Reference: [OpenAI API reference](https://developers.openai.com/api/reference/overview),
[Responses create](https://developers.openai.com/api/reference/resources/responses/methods/create).

## Anthropic

| Endpoint or feature | OLR coverage |
|---|---|
| `POST /v1/messages` | Native JSON and SSE, including tool use/results, thinking and provider fields when the backend supports them. |
| `POST /v1/messages/count_tokens` | Forwarded natively; token-count requests do not become generation usage charges. |
| `anthropic-version`, `anthropic-beta`, cache controls | Forwarded; acceptance and cache hits depend on the backend. |
| Native Models list/retrieve | Not implemented. `GET /v1/models` currently returns OpenAI's schema; it lacks Anthropic model metadata and pagination. `GET /v1/models/{id}` is not registered. |
| Message Batches, Files, Skills, managed agents/sessions, organization/admin APIs | Not implemented. |
| Provider-hosted tools | Passed through in Messages only when supported by the backend. OLR does not supply a web-search or code-execution service. |

Reference: [Claude API overview](https://platform.claude.com/docs/en/api/overview),
[native Models list](https://platform.claude.com/docs/en/api/http/beta/models/list),
[model retrieval](https://platform.claude.com/docs/en/api/http/beta/models/retrieve).

## What compatibility guarantees

Supported inference routes share OLR authentication, model/alias and group checks,
backend balancing, capacity/queue handling, limits, credential isolation, deadlines,
usage accounting and consent-controlled tracing. Request bodies use the configured
size limit (4 MiB by default). Client credentials are replaced with the selected
backend's configured bearer credential; OLR does not proxy provider account keys
or provider-owned resource permissions from the client.

Unknown request fields and protocol headers pass through on supported routes.
This does not establish support for every feature: an upstream may reject a tool,
model, API version, content type or parameter. A healthy backend is not proof that
it implements Responses or Anthropic Messages. Pools must contain replicas with
the same native API and model capabilities.

Chat Completions and Responses clients may explicitly request the
`X-OLR-Usage-Timing: true` usage extension. It adds router-observed throughput
without replacing provider fields or changing token accounting. The header is
consumed by OLR; native bodies remain unchanged by default. See
[Open WebUI setup and measurement semantics](CLIENTS.md#open-webui-response-throughput).

Responses create is deliberately stateless. Stored identifiers on shared backend
credentials require user ownership checks and replica affinity; background jobs
also require asynchronous usage and concurrency reconciliation. Those lifecycles
must be implemented together before exposing their endpoints.

## Response transformations and provenance

By default supported JSON response bodies and retained SSE events preserve
upstream bytes, including supplemental provenance fields. The exception is an
ordinary OpenAI Chat Completions/legacy Completions usage-only event requested
by OLR: with empty `choices` and valid counts, it can be suppressed if the caller
did not request usage. Only recognized envelope fields (`id`, `object`, `created`,
`model`, `system_fingerprint`, `service_tier`, `choices`, `usage`) and standard
token counts/details qualify;
unknown fields, nested usage extensions, duplicate keys and additional SSE
fields/comments prevent suppression (the default `event: message` is allowed). An event with supplemental information is
forwarded whole, including its usage, even when the caller set `include_usage:false`.
Responses and Anthropic events do not use this suppression.

This is a body/event preservation guarantee, not a byte-for-byte HTTP exchange.
Hop-by-hop headers and `Set-Cookie` are removed; request IDs, cache/buffering and
CORS headers are controlled by OLR, and framing can change. Explicit
`X-OLR-Usage-Timing:true` augments usage objects and removes affected integrity/length
headers. Interrupted streams can be truncated or aborted. Requests may be rewritten
for aliases, upstream model names, priority, Responses state defaults and usage
collection. Personal traces omit/redact content and metadata and have size limits.
Clients, reverse proxies and exports can make further transformations.

A model name, site notice or header alone cannot establish that an integrated
AI service meets Article 50(2). That provision concerns machine-readable marking
and detectability of generated/manipulated outputs and the effectiveness,
interoperability, robustness and reliability of the technical solution. OLR's
transport tests do not establish those properties. Assess the deployed upstream
mechanism and the complete client/export path using the
[end-to-end checks](CLIENTS.md#upstream-provenance-checks), with the applicable
scope and exceptions in [Article 50 of the AI Act](https://eur-lex.europa.eu/eli/reg/2024/1689).

## Verification and next gaps

`make sdk` runs pinned official OpenAI and Anthropic SDKs against OLR and both
deterministic mocks. Responses checks cover JSON, SSE, full-history tool loops
(streaming and non-streaming), JSON schema output, long output and cancellation
with backend abort/inflight evidence. The existing suites cover Chat Completions,
Completions, embeddings, Models, Anthropic Messages/token counting, thinking and
beta/cache-field acceptance. Unit and PostgreSQL integration tests cover policy,
usage persistence, quotas and interrupted-stream accounting.

These checks verify the router and fixtures. They do not certify all API schemas,
provider-hosted features, the deployed GPU backends or a live Open WebUI session.
For Open WebUI setup and separate upstream checks, see
[CLIENTS.md](CLIENTS.md#open-webui-responses-connections).

The next compatibility gaps to consider are native Anthropic model discovery and
access-filtered model retrieval, then Responses compact/input-token endpoints
where the selected backend supports them. Stored resources and asynchronous APIs
need a separate ownership, routing and accounting design.
