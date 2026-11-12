# Production pilot

Issue [#21](https://github.com/arm2arm/open-llm-router/issues/21) requires a production
Keycloak realm, real GPU pools and two weeks of real user activity. Repository tests and
local CPU/Metal backend results establish readiness; the site records the production
evidence below before closing the issue. The delivery milestones in
[#1](https://github.com/arm2arm/open-llm-router/issues/1) are complete through M5;
the production pilot is M6.

## Site decisions

Keep an owner and the agreed value for each row in the site's private deployment record.
No production credentials, user identities or backend addresses belong in GitHub.

| Decision | Required before starting |
|---|---|
| Identity | Realm issuer; owner of `olr-portal`, `olr-api`, `olr-sync`; groups mapper; brokered IdP and required claim if used |
| Host and network | Compose host, public HTTPS origin, TLS/proxy owner, router-to-Postgres/IdP/backend firewall rules |
| Models and pools | Served model and protocols; each replica's origin, credential, measured weight and `max_inflight` |
| Policy | Exact group claim values, model access, daily/monthly quotas, RPM, TPM and concurrency |
| Privacy | Record/daily-total/log/trace/backup retention; published notice owner and erasure contact |
| Operations | Pilot coordinator, on-call owner, dashboard/alerts, backup and restore owner, rollback image |

For `olr-portal`, register the exact HTTPS `/portal/callback` redirect and `/portal/`
post-logout redirect, confidential client authentication, PKCE S256 and the groups mapper.
For service access tokens, map `olr-api` into the access-token audience and supply groups.
Give `olr-sync` a service account with `realm-management` `view-users`; Keycloak 26.8+
also requires its `kc-admin-api` client scope. See the
[service-token and group-sync runbook](RUNBOOK.md#service-tokens-and-group-sync-keycloak) for
service-token and group-sync setup. Check mapped claims against the private group policy
before onboarding; an unmatched group gets no model access.

## Private Compose deployment

Use [pilot.yml](../deploy/compose/pilot.yml) as a standalone Compose file. It runs one
router image with external PostgreSQL, Keycloak and GPU replicas. It binds only to
loopback for the host's TLS proxy, forces secure cookies, clears the development
backchannel, requires site credentials, and mounts the configuration read-only. The image
runs as non-root with a read-only filesystem and no Linux capabilities.

From the repository root, prepare a directory outside the checkout:

```sh
umask 077
mkdir -p "$HOME/.config/olr-pilot"
cp deploy/compose/pilot.env.example "$HOME/.config/olr-pilot/pilot.env"
cp deploy/compose/pilot.config.example.yaml "$HOME/.config/olr-pilot/config.yaml"
chmod 700 "$HOME/.config/olr-pilot"
chmod 600 "$HOME/.config/olr-pilot/pilot.env"
# The container UID 65532 must read this bind mount. Its parent remains private.
chmod 644 "$HOME/.config/olr-pilot/config.yaml"
export OLR_PILOT_ENV_FILE="$HOME/.config/olr-pilot/pilot.env"
```

Edit both copies. Set `OLR_SITE_CONFIG` to the absolute config path and `OLR_IMAGE` to
the tested `registry/image@sha256:...` digest. Set an HTTPS `OIDC_ISSUER` and
`PORTAL_BASE_URL`, the production database URL and all secrets. Single-quote values
containing `$`, `#` or spaces in the environment file. Use the site's required database TLS
settings. Both config examples assume authenticated vLLM replicas; remove a replica's
`bearer_token_env` when it intentionally has no authentication. Adapt the replica count,
protocol list and groups to the site. The quota/capacity values supplied are starting
examples and need site agreement and measurement.

The environment file is passed to both Compose interpolation and the container so that
custom backend credentials and YAML variables reach the router. Shell environment values
take precedence during Compose interpolation; use a clean deployment shell without dev
overrides. Keep the private files and any rendered Compose output out of Git.

```sh
docker compose -f deploy/compose/pilot.yml --env-file "$OLR_PILOT_ENV_FILE" config --quiet
docker compose -f deploy/compose/pilot.yml --env-file "$OLR_PILOT_ENV_FILE" pull
docker compose -f deploy/compose/pilot.yml --env-file "$OLR_PILOT_ENV_FILE" up -d
```

Do not merge the development Compose file into this deployment. Startup validates the
mounted YAML, migrates Postgres and checks OIDC. Container startup alone is insufficient:
poll `/healthz` and `/readyz` through the TLS origin until both return 200, with a bounded
timeout. Then verify a streamed completion. The distroless image has no shell or curl for
an in-container health check. Size-based Docker log rotation bounds disk use; configure
age-based retention separately to meet the published notice.

Configure the host TLS proxy using [RUNBOOK.md](RUNBOOK.md#behind-a-tls-reverse-proxy),
including buffering off, body limits and timeouts. Scrape `/metrics` through a private
route with its separate bearer token. Prepare the dashboard and alerts described in
[OBSERVABILITY.md](OBSERVABILITY.md). Take a database backup before upgrades and follow
the runbook's restore procedure when rolling back a schema change.

## Acceptance before onboarding

Record timestamps, the image digest, sanitized configuration revisions and pass/fail
results in the private pilot record. Publish only a redacted summary in #21.

1. Verify the production issuer's discovery and login, mapped groups and logout through
   the public TLS origin. Create a key, use it and verify its usage in the portal. Test
   both an allowed pilot account and an account without the required group/claim. Confirm
   service-token audience checks and group sync, including disabling a disposable account.
2. Record the digest before and after setting the production `OIDC_*` values. They must
   match: switching identity providers requires configuration only, with no source or
   image rebuild. Configuration validation alone does not establish this result.
3. Follow [CLIENTS.md](CLIENTS.md#sdk-runs-and-cancellation-evidence) for each deployed GPU/model/protocol
   combination, direct and routed: set `OLR_BASE_URL` to the pilot's HTTPS origin,
   `OLR_DIRECT_BASE_URL` to the replica origin, `OLR_UPSTREAM_MODEL` to the served name,
   `OLR_MODEL=chat-model`, and separate router/backend test keys. Run `make sdk-real`,
   `make bench-real` and one tool-using
   session each with Claude Code and OpenCode. Use a dedicated test key/group with enough
   quota, and set `OLR_REQUIRE_CANCEL_EVIDENCE=1` with backend cancellation evidence. Save
   failures as well as passes. Cancellation checks require one idle replica and no other
   traffic: temporarily select only that replica in the private test pool, then restore and
   verify the final pool before onboarding. Enable only protocols that pass against the deployed build.
4. Check a long stream through TLS for prompt first content, unbuffered chunks and no
   proxy timeout. Verify cancellation reaches the backend, pool failover, admission limits,
   and matching per-request usage. Exercise saturation only in an agreed test window;
   the example pilot concurrency limits cannot support the 256-client load test unchanged.
   The runbook's [existing-backend load test](RUNBOOK.md#existing-backends) documents a
   dedicated key, direct replicas and read-only database verification.
5. Confirm each replica becomes healthy, a protected metrics scrape succeeds, and an
   unauthenticated scrape is refused. Alert on readiness loss, sustained errors and any
   increase in `olr_usage_dropped_total` or `olr_usage_write_failures_total`. Confirm backups,
   retention runs, self-service activity deletion and operator account erasure with a
   disposable account. Publish the completed privacy notice before inviting users.

## Two-week observation and closure

The coordinator records the first real pilot activity and a review date at least 14 days
later. A running container or synthetic requests alone do not meet the activity criterion.
Keep daily aggregate activity, model/replica health, error/limit rates, latency, token
throughput and accounting-loss observations. Review client reports regularly, file
reproducible problems with redacted evidence, and assign severity, an owner and disposition.
Stop onboarding and investigate if accounting is lost or an access-control problem appears.

Use this closure record in #21 after the observation period:

- [ ] Site decisions have owners and approved values; private deployment record identified.
- [ ] Production IdP login and GPU/client acceptance passed, with a redacted evidence link.
- [ ] Image digest before/after the IdP switch is identical and recorded.
- [ ] Real pilot users were active for two weeks: start date, end date and aggregate activity.
- [ ] Problems found are linked to issues and triaged; unresolved risks have an owner.
- [ ] Operations/privacy owners reviewed the results and recorded the pilot disposition.

Leave #21 open while any required evidence is missing. Local test results must be labelled
as local results; they cannot be used to check the production or elapsed-time boxes.
