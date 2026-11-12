# Documentation screenshots

These are captures of the real embedded portal, using the implementation from
PRs [#92](https://github.com/arm2arm/open-llm-router/pull/92) and
[#96](https://github.com/arm2arm/open-llm-router/pull/96) (November 2026).
Accounts, email addresses, usage totals and message content are synthetic
demonstration data. Request traces come from real calls through the disposable
router to the development stack's mock backends. Timings are illustrative, not
production records or benchmark results. The healthy replicas are those mocks.

| Image | What it shows |
|---|---|
| `admin-overview.png` | Endpoint configuration and replica health |
| `endpoint-editor.png` | Remote model discovery, protocol settings and an unsaved one-replica draft |
| `portal-usage.png` | A regular user's allowances and daily usage chart |
| `sign-in.png` | Keycloak and independent local login |
| `portal-requests.png` | Detailed logging, seven-day default retention, request types, status and timings |
| `request-trace-conversation.png` | Input/output messages, reasoning, tool arguments and submitted tool results |
| `request-trace-events.png` | Completed-request stream timeline with reasoning and tool argument deltas |
| `request-trace-details.png` | Request ID, operation, expiry and selected generation parameters |

The observability fixture submits OpenAI chat calls with and without streaming,
an embedding request, an intentional unknown-model 404, and a streamed Anthropic
call with thinking and a tool definition. Earlier tool calls/results are supplied
as input messages. The mock returns canned thinking and a new tool call; the
router does not execute the tool. Older usage-only rows are seeded metadata.

## Refresh the images

Start the normal development Compose stack with the current router image and
bundled Keycloak on port 8081. Then, from the repository root:

```sh
cd web
npm ci
npx playwright install chromium
node scripts/capture-readme.mjs
```

To refresh only the four observability images:

```sh
node scripts/capture-readme.mjs --observability-only
```

For a different Compose project, set `COMPOSE_PROJECT_NAME`. The script uses that
project's Postgres service, network, mock replicas and router image. It creates a
separate `readme_demo_*` database and router container, completes the demo local
password changes, seeds synthetic records, makes the example API calls and
captures the UI with Chromium. It checks that all five new calls have detail,
capture is enabled with seven-day retention, the tool/reasoning views are present,
and no browser page errors occurred.
The temporary browser, database and container are removed in `finally` cleanup.
The existing router, accounts, sessions and saved endpoint revisions are untouched.

Screenshots contain no full API keys, passwords, cookies or private deployment
addresses. Keys created for the demo are discarded with its database. No API
responses or page content are replaced to construct the screenshots.
