# Operations runbook

## Erase a user on request

Find the user, then delete them. Deletion removes the account with its keys, sessions,
per-request records (including calls a trusted forwarder made for the user) and daily totals
in one transaction, clears the API-key caches of every router before returning, and cannot
be undone:

```sh
docker compose exec -T router olr admin user list -match alice
docker compose exec -T router olr admin user delete -id <uuid>
```

A user who signs in again afterwards gets a new, empty account. To block access instead, use
`olr admin user disable -id <uuid>`. Request log lines and exported spans are outside the
database; see [PRIVACY.md](PRIVACY.md).

## Service tokens and group sync (Keycloak)

**Access tokens on `/v1/*`.** With `oidc.audience` (`OIDC_AUDIENCE`, e.g. `olr-api`) set, a
bearer that is not an API key is checked as an access token of the issuer: signature against
the JWKS (refetched when a token names an unknown key), `iss`, `aud` containing the audience,
`exp`. The token's subject becomes a router user (issuer + `sub`) with the token's groups, so
quotas and usage apply as for any user. A service obtains tokens with client credentials:

```sh
curl -s "$OIDC_ISSUER/protocol/openid-connect/token" -d grant_type=client_credentials \
  -d client_id=olr-service -d client_secret=... | jq -r .access_token
```

In Keycloak create a client `olr-api` (no flows; it is only the audience) and give each service
client an *audience* mapper that adds `olr-api` to access tokens, plus the groups mapper. The
dev realm has `olr-service` as an example. A user disabled in the router is refused within a
minute (verified tokens are cached for at most a minute).

**Group sync.** A confidential client `olr-sync` with *service accounts* enabled, the
`realm-management` role `view-users`, and on Keycloak 26.8 or later the `kc-admin-api`
client scope (its tokens must carry the admin-API audience). Set its secret in
`OIDC_SYNC_CLIENT_SECRET`. At startup and every `oidc.sync_interval_s` (default 900) one
router checks every enabled user of the issuer: removed or disabled in Keycloak → disabled here
with their keys and sessions; changed groups → stored, effective for existing keys at once.
Users are not re-enabled automatically: use `olr admin user list` and the portal's admin page,
or `PUT /portal/api/admin/users/{id}/enabled`. Each run logs `msg="group sync"` with counts, or
`group sync failed`.

## Open WebUI and other trusted forwarders

An application that serves many users with one key can attribute each call to its user:

1. Create a service user and a forwarder key:
   `olr admin user add -username open-webui -groups staff`, then
   `olr admin key create -user <uuid> -label open-webui -trusted-forwarder`.
2. Map the header the application sends, in the config file:
   `forwarders: {header_map: {X-OpenWebUI-User-Email: email}, unknown_user: reject}`.
   `X-OLR-End-User: <username>` always works.
3. In Open WebUI set `ENABLE_FORWARD_USER_INFO_HEADERS=true` and add the router as an OpenAI
   connection (`<router>/v1`) with the forwarder key.

Each call with the header is authorized against the end user's groups, counts against their
limits and quotas, and is recorded with `usage.forwarded_for` set to them, so it appears in
their portal and they can delete it. Users must exist in the router — they have signed in to
the portal once, or were added with `olr admin user add` — and their email must be unique.
Otherwise the call is refused with 403 (`unknown_user: reject`) or served as the forwarder's
own (`forwarder_policy`). A disabled user is always refused. The mapped headers are removed
before the call reaches a backend, and ignored from keys without the flag.

## Usage retention

The router deletes per-request records older than `retention.usage_days` (default 90) and
daily totals older than `retention.usage_daily_days` (default 400, at least 62 so that quotas
keep the current month) once at startup and every night at `retention.run_at` in
`limits.timezone`. Deletion runs in batches of 10,000 rows; with several routers, one deletes
while the others skip. Each run logs `msg=retention` with the number of deleted rows, or
`retention job failed`. To run it by hand:

```sh
docker compose exec -T router olr admin retention run   # prints {"usage":N,"usage_daily":M}
```

`usage` is one table indexed by time. Monthly partitions are not needed at pilot volume
(roughly 3 GB with indexes per 10 million requests); they can be added by a later migration if the table
grows too large to prune by index.

## Database migrations

Migrations in `internal/db/migrations` run at startup inside one transaction under an advisory
lock, so concurrent routers are safe and a failed migration changes nothing. `olr migrate`
applies them and prints the schema version, for a job that runs before the routers start.
Take a backup before upgrading; a router refuses a database with a newer schema, so roll back
by restoring the backup together with the previous image.

## Streaming load test

`cmd/olr-loadtest` compares identical synthetic streaming workloads directly against
the backends and through the router, separately for OpenAI and Anthropic. It also
checks cancellation, accounting and idle runtime state.

### Reproduce with Compose

Docker Compose is the only host dependency:

```sh
make loadtest > loadtest.json
```

The script creates a unique `olr-loadtest-<uid>-<pid>` project with a private network,
a disposable Postgres database, two mocks, a router and the load generator. It
creates a dedicated user and key, then removes that project and its data on exit.
There are no published ports. An existing development stack remains running.
Public test credentials apply only to this isolated stack. Secrets are passed in
environment variables and are absent from reports and command-line arguments.

The benchmark config is [loadtest.config.yaml](../deploy/compose/loadtest.config.yaml),
separate from the pilot's much smaller limits. Replica weights are 2:1, with 5 ms
and 10 ms token delays. Each replica has 8,192 slots; the user has 16,384 concurrency
slots and no RPM or token quota. These capacities let the experiment reach the
router's runtime/accounting limit instead of the pilot's admission limits.

The router and generator each have a four-CPU quota and 1 GiB memory limit; each
mock and Postgres have two CPUs and 512 MiB. The script reduces CPU quotas on hosts
with fewer CPUs. `OLR_LOADTEST_CPUS` and `OLR_LOADTEST_AUX_CPUS` can select a smaller
budget; record the values with any published result. Run on an otherwise quiet
machine. Shared CI or Docker Desktop scheduling can affect timing.

The JSON report includes per-protocol/per-run measurements, error categories,
intentional disconnect counts, replica distribution, CPU cores used, peak
RSS/heap/goroutines, and idle snapshots. A nonzero exit status means a check failed
or the experiment could not complete. Successful HTTP responses alone do not prove
that accounting or cancellation worked.

### Measurement and checks

- Both paths are warmed at full concurrency before measurement. A synchronized
  worker pool maintains up to `-concurrency` active clients until `-requests` finish.
  The direct path uses the configured weights and least-outstanding selection;
  direct and router order alternates across runs.
- Time to first token stops at the first nonempty content delta, excluding role
  and message-start events. `AddedP95TTFTMS` is `p95(router) - p95(direct)`, the
  difference of two distributions, not a paired per-request measurement. Its
  default budget is below 5 ms.
- Per-stream tokens/s is `(output_tokens - 1) / (last_content - first_content)`;
  aggregate tokens/s is completed output tokens divided by batch wall time.
  Both the median per-stream ratio and aggregate router/direct ratio must be at
  least 0.98. A terminal event and token usage are required; truncated responses,
  SSE error events, non-SSE responses and HTTP failures are errors.
- A separate batch disconnects 10% of clients after four content deltas. These
  calls are excluded from performance statistics. Mock cancellation counters
  must increase by at least the number of intentional disconnects.
- Replica gauges, mock in-flight counters and `/v1/me/usage.in_flight` must return
  to zero after each run. The tool waits for asynchronous persistence and verifies
  one `usage` row per router request for the dedicated key, including warm-ups and
  disconnects. No writer failures or drops are allowed.
- Client keep-alives are closed before idle snapshots. The initial warmed state
  is recorded separately from the post-run snapshots, because the upstream idle
  connection pool can still grow during the first measured run. Across three
  post-run snapshots, goroutine spread is at most four (health checks and scrapes).
  Across three runs, heap spread must be no more than the larger of 32 MiB or 25%; RSS
  spread no more than 64 MiB or 25%. Raw values remain visible: finite-run bounds
  detect regressions, not every possible leak.

Defaults are 256 clients, 128 output tokens, twice as many requests as clients,
three runs and two seconds of quiet time before each idle snapshot. For longer
generations and one simultaneous wave per batch:

```sh
make loadtest LOADTEST_ARGS='-requests 256 -tokens 256' > loadtest-long.json
```

CI runs the same 256-client functional, accounting and leak checks with 32 tokens
and `-check-performance=false`. It preserves the JSON artifact, including measured
performance. Shared-runner timing is recorded without making it a pass/fail gate;
all functional checks remain enforced.

### Find the capacity boundary

Raise concurrency and remove the mock's generation delay, keeping resource quotas
fixed. Disconnections are tested in the paced run; disable them for this maximum
token-rate experiment because a mock may finish before a client can disconnect.

```sh
for n in 64 128 256 512 1024; do
  make loadtest LOADTEST_ARGS="-concurrency $n -requests $((n * 4)) -tokens 128 -mock-delay 0s -abort-fraction 0 -runs 1 -settle 1s -check-performance=false" \
    > "loadtest-fast-$n.json" || true
done
```

Each iteration is isolated and retains failures. Compare direct and router
throughput, router CPU, errors, usage drops and persisted rows. A generation-rate
cap in the mocks or a configured pool/user limit is not router saturation.
A router CPU plateau with faster direct throughput, or a full accounting queue
with missing rows, identifies a router-side boundary. Keep the load generator and
mocks out of CPU throttling before assigning a CPU limit to the router. Results
are specific to the recorded resource budget and workload.

### Existing backends

`make build` also builds `bin/olr-loadtest`. Set `OLR_API_KEY`, a separate
`OLR_METRICS_TOKEN`, and optionally `OLR_DIRECT_API_KEY` in the environment. Set
`OLR_LOADTEST_DATABASE_URL` to verify usage with a read-only role that can read
`api_keys` and `usage`. Omit `-mock` so no mock control headers are sent:

```sh
bin/olr-loadtest -router https://router.example.org \
  -direct https://replica-a.example.org,https://replica-b.example.org \
  -weights 2,1 -model chat-model -direct-model backend-model \
  -concurrency 256 -tokens 512 > loadtest-real.json
```

Use a dedicated key and sufficient limits. The tool does not modify configuration
or erase existing records. Without database credentials, `AccountingVerified`
stays false and the report states that limitation. Fewer than three repetitions
leave `LeakStabilityVerified` false and skip stability assertions. Real GPU/backend
and client validation remains tracked in
[#8](https://github.com/arm2arm/open-llm-router/issues/8).

### Recorded results

Measurements for [#18](https://github.com/arm2arm/open-llm-router/issues/18), including
the capacity boundary, are recorded below. The development stack on port 8080 is
not part of these experiments.

Recorded on 11 November 2026 with Go 1.27.4, Linux/arm64 containers on Docker
Desktop (18 vCPUs and 64 GiB available to the VM). Resource quotas are the Compose
defaults above: router/generator four CPUs each, mocks/Postgres two CPUs each.
These are synthetic local measurements, not a real-GPU capacity claim.

Strict acceptance command: `make loadtest > loadtest.json`. All six measured
batches reached 256 overlapping clients and completed without errors. All checks
passed, including both performance ratios on every repetition.

| Protocol | Run | Added p95 TTFT (ms) | Median tokens/s ratio | Aggregate tokens/s ratio |
|---|---:|---:|---:|---:|
| openai | 1 | 3.990 | 0.9988 | 1.0171 |
| anthropic | 1 | -2.051 | 1.0095 | 1.0091 |
| openai | 2 | 0.283 | 0.9943 | 1.0215 |
| anthropic | 2 | -0.653 | 1.0080 | 1.0095 |
| openai | 3 | 1.993 | 0.9801 | 1.0024 |
| anthropic | 3 | -0.835 | 0.9894 | 1.0282 |

The worst added p95 TTFT was 3.990 ms; the worst median rate ratio was 0.9801
(1.99% loss). Aggregate throughput was at least 1.0024 times direct throughput.
The router persisted all 6,656 requests, including 150 intentional disconnects,
with zero writer failures or drops. Every post-run replica/user/mock in-flight
counter was zero. Replica A handled 4,090 requests and B 2,054 after the baseline
(66.6% / 33.4%, matching the configured 2:1 weights).

Idle goroutines were 484, 486 and 486 after the three runs (533 at the initial
warmed snapshot). Idle RSS was 35.2, 55.3 and 59.0 MiB; heap in use was 10.5, 13.3
and 34.8 MiB. Both remain within the stated finite-run bounds. Peak RSS was 59.5
MiB and sampled peak router CPU was 1.04 cores. The full measurements are in
[the acceptance report](benchmarks/2026-11-11-arm64-256.json).

A regression found during this work was the expired-key lookup burst: 256
simultaneous requests could issue 256 identical database lookups. Refreshes now
coalesce to one lookup while the invalidation listener is healthy. Regression
tests also cover owner/waiter cancellation, revocation during refresh, and the
existing fail-closed behavior when the listener is lost.

The zero-delay experiment used four requests per client, 128 tokens, one run,
no intentional disconnections and performance gates disabled. This profile
finds the accounting boundary; it does not establish multi-run leak stability.

| Clients | Router requests | Persisted rows | Dropped rows | HTTP/stream errors | Sampled peak CPU (cores) |
|---:|---:|---:|---:|---:|---:|
| 256 | 3,072 | 3,072 | 0 | 0 | 2.48 |
| 512 | 6,144 | 5,454 | 690 | 0 | 3.30 |

**Observed boundary: the 1,024-record accounting queue starts dropping records
between 256 and 512 simultaneous clients in this unpaced burst profile.** At 512
clients the inference path still completed every call, but 690 of 6,144 records
were dropped. This is a router accounting bottleneck, not a mock generation-rate
limit: the queue-drop counter increased while all inference streams completed.
A prior exploratory 1,024-client run also dropped records. This is not a universal
maximum client count: token rate, burst shape, Postgres latency, pool limits and
resource quotas change the boundary. Do not size a deployment from successful
HTTP responses alone. Alert on `olr_usage_dropped_total` and
`olr_usage_write_failures_total`; reduce admission or improve persistence capacity
before running an equivalent sustained workload.

Raw capacity reports: [256 clients](benchmarks/2026-11-11-arm64-fast-256.json),
[512 clients](benchmarks/2026-11-11-arm64-fast-512.json). The 512-client command
correctly exits nonzero for dropped accounting; it is not an acceptance pass.
No reachable real vLLM/GPU backend was configured for this session; repeat on the
deployment hardware before the pilot.
