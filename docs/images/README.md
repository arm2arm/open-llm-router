# README screenshots

These are captures of the real embedded portal, using the implementation from
PR #92 (November 2026). Accounts, email addresses, usage totals and timing values
are synthetic demonstration data, not production records or benchmark results.
The healthy replicas are the development stack's mock backends.

| Image | What it shows |
|---|---|
| `admin-overview.png` | Endpoint configuration and replica health |
| `endpoint-editor.png` | Remote model discovery, protocol settings and an unsaved one-replica draft |
| `portal-usage.png` | A regular user's allowances and daily usage chart |
| `sign-in.png` | Keycloak and independent local login |

## Refresh the images

Start the normal development Compose stack with the current router image and
bundled Keycloak on port 8081. Then, from the repository root:

```sh
cd web
npm ci
npx playwright install chromium
node scripts/capture-readme.mjs
```

For a different Compose project, set `COMPOSE_PROJECT_NAME`. The script uses that
project's Postgres service, network, mock replicas and router image. It creates a
separate `readme_demo_*` database and router container, completes the demo local
password changes, seeds synthetic records and captures the UI with Chromium.
The temporary browser, database and container are removed in `finally` cleanup.
The existing router, accounts, sessions and saved endpoint revisions are untouched.

Screenshots contain no full API keys, passwords, cookies or private deployment
addresses. Keys created for the demo are discarded with its database. No API
responses or page content are replaced to construct the screenshots.
