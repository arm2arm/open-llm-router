# Portal development

Node 22.23.1+ and npm are needed. From the repository root, `make web-check`
installs the lockfile, lints, type-checks, builds, and audits dependencies.
`make build` builds this app before embedding it in the Go binary. The Docker
build does the same in a Node stage; the final distroless image contains only Go.
Generated `dist` files stay out of Git. Its tracked `.keep` allows API-only Go
development before a frontend build; use `make build` for a complete binary.

Start Compose and visit `http://localhost:8080/portal/`. The default staff,
student and admin logins are documented in the root README. `npm run test:e2e`
runs the Chromium journey against that stack, including a completion from a
copied curl snippet and usage totals checked directly in Postgres. Install the
browser once with `npx playwright install chromium`. Override `OLR_BASE_URL`
and `COMPOSE_PROJECT_NAME` when using different ports/a separate stack.
The test disables screenshots, video and traces because a new API key is
displayed during the journey. No test artifacts are uploaded in CI.

For hot reload, run the router with `PORTAL_BASE_URL=http://localhost:5173`,
`PORTAL_COOKIE_SECURE=false`, and register `http://localhost:5173/portal/callback`
and `http://localhost:5173/` as the post-logout redirect in your development OIDC client. The
Compose realm gets these from `PORTAL_BASE_URL` when first imported; use a fresh
development Keycloak volume or update the client when changing the URL later.
Then run `npm run dev`. `OLR_DEV_ROUTER` optionally changes the proxy destination
from `http://localhost:8080`. Vite proxies only `/portal/api`, `/portal/login`,
`/portal/callback`, `/portal/logout`. Browser Origin is preserved for CSRF checks;
it must match the router's configured public portal origin.

The app uses React Router under `/portal/` and TanStack Query for server state.
Generated shadcn/ui primitives use Radix, local Tailwind theme tokens and Lucide
icons. Query caches contain metadata, never full keys or OIDC tokens. A new key
exists only in dialog state and clears on close/unmount/reload; neither browser
storage nor snippets receive it. Themes follow the system on load and can be
toggled for the current page. CSP allows scripts from the same origin only;
inline styles support Radix's focus/scroll management.

Client snippets follow the versions/settings tested in `docs/CLIENTS.md`.
OpenCode limits are explicit examples and must match the site's backend.

Administration acceptance is part of `local-auth.spec.ts`: local-only and bundled
SSO, a fresh external Keycloak process/realm with connection failures and rollback,
and a real PostgreSQL dump/restore followed by lost-password recovery. Every
fixture owns its database, container and public synthetic credentials. No dump,
credential-bearing screenshot, video or trace becomes a test artifact. The dump
is restored from process memory, and the source database is replaced only after
stopping its disposable router. `make acceptance-local` creates and cleans a
separate Compose project without starting Keycloak. CI runs both the complete
bundled/external suite and that independent no-IdP installation.

## TypeScript compiler and lint API

`npm run typecheck` and the production build use TypeScript 7's `tsc`, installed
through the pinned `@typescript/native` npm alias. ESLint imports `typescript`,
which resolves to the official `@typescript/typescript6` compatibility package.
TypeScript 7 does not provide the compiler API required by the current ESLint
parser. Keep both aliases when updating the lockfile so clean installs preserve
peer dependency checks and select the native compiler consistently. This follows
[Microsoft's side-by-side migration guidance](https://devblogs.microsoft.com/typescript/announcing-typescript-7-0/).
