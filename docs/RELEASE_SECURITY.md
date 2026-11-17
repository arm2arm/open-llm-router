# Release and repository controls

Production deployments use immutable image digests and verify who built and signed
each image. Local/demo builds remain available without publishing a release.

## Publish a signed release

Run **Signed release** (`.github/workflows/release.yml`) manually on `main`, with an
unused `vMAJOR.MINOR.PATCH` version. It refuses other branches and any commit without
a successful main CI run. The workflow builds the router image in GHCR, produces
BuildKit SBOM/provenance and an SPDX SBOM, signs the image with keyless Cosign, and
publishes signed GitHub attestations for both provenance and the SPDX document.
It verifies those signatures and attestations before creating a Gitsign-signed
annotated tag and a GitHub release containing `image.txt` and `sbom.spdx.json`.

The workflow publishes `linux/amd64` and `linux/arm64` together. `image.txt` records
the OCI index digest; both verification wrappers accept that immutable digest.
Build stages run npm and Go on the builder's native platform and cross-compile the
target binary, so building ARM64 on an AMD64 runner needs no QEMU. CI builds and
inspects that non-native image, including the ELF architecture, labels and notices.
Older published versions may contain only AMD64; inspect their manifest before use.

`olr version` prints the release version, commit and Go version without connecting
to a database. The release build supplies the input tag and source commit, verifies
the published image's output, and sets matching OCI version/revision/source labels.
The authenticated metrics endpoint exposes the same identity in `olr_build_info`.
Local builds identify themselves as `dev` / `unknown` unless build arguments are set.

The router image includes its MIT license at `/licenses/LICENSE`, Go's license,
original dependency license/NOTICE files, and `/licenses/THIRD_PARTY_NOTICES.txt`
covering linked Go dependencies and the portal production dependency closure.
Builds check a permissive SPDX allow-list and fail on unreviewed licenses or missing
texts. The npm generator has negative license/missing-text tests; Docker builds run
the Go check and collect original notices. See `web/licenses/README.md` for the one
version-specific upstream license omitted by an npm tarball. The OCI license label
describes the router's MIT license; dependency notices retain their own terms.

The trusted certificate identity is the exact release workflow on `refs/heads/main`,
with issuer `https://token.actions.githubusercontent.com`. Signing uses the runner's
short-lived OIDC identity. No long-lived signing key is stored. No release is created
by merging this workflow; maintainers choose the version and dispatch it separately.
A failed publication may leave a signed image or tag. Investigate it before retrying;
the workflow refuses to replace existing Git tags or an existing GHCR image version,
including a version left by a failed publication. Registry lookup failures stop the
release before building. Choose a new version after investigating a partial release.

The image build/sign/attestation job has read-only repository access. A separate
read-only job compiles the pinned Gitsign tool; the final publishing job receives
that immutable, same-run artifact and verified release files. Only the publishing
job can write Git tags and releases. `GH_TOKEN` is supplied only to steps using the
GitHub or registry API. No release workflow is dispatched by these policy tests.

Gitsign tags use X.509 Sigstore signatures. Verify the tag's identity explicitly;
GitHub's verified badge and ordinary `git verify-tag` do not establish this policy:

```sh
git fetch origin --tags
gitsign verify-tag --certificate-identity \
  https://github.com/arm2arm/open-llm-router/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com vX.Y.Z
```

## Verify before deploying

Install Helm 4, Python 3 with PyYAML 6.0.3, Cosign 3.1.4+, a current GitHub CLI with
`gh attestation verify`, and kubectl. Authenticate to GHCR/GitHub as needed. Copy
`charts/open-llm-router/examples/production.yaml` into private site configuration,
set `image.repository: ghcr.io/arm2arm/open-llm-router` and the verified digest from
the release's `image.txt`, then configure TLS, network peers and site secrets.

```sh
deploy/release/helm-verified.sh olr olr /private/site.yaml deploy/release/image-policy.json
# Or render the site's production Kustomize overlay and apply the verified YAML:
deploy/release/kubectl-verified.sh /private/olr-overlay deploy/release/image-policy.json
```

The verified Helm wrapper explicitly sets `production=true`, `enforceImageDigests=true`
and `config.security.require_tls=true`. These reject missing image digests, mutable
enabled supporting images, disabled network policy and unverified transport. Ordinary
chart installs allow tags and default to `require_tls: false`; `production` alone does
not force either policy. Supplied digests must always have valid SHA-256 syntax.
Bundled demo services are incompatible with explicitly enabled end-to-end TLS;
use external managed PostgreSQL and identity services. PostgreSQL and Keycloak demo
defaults are digest-pinned too, including the PostgreSQL init container. Demo backend
images support a separate digest override.

Both wrappers verify every rendered container, init container and ephemeral container
before allowing any YAML to be applied. The exact image digest must have a valid
Cosign signature plus signed SLSA provenance and SPDX SBOM attestations from the exact
repository/workflow identity in `deploy/release/image-policy.json`. Self-hosted builders
and source refs other than main are rejected. Any missing policy, mutable image,
unavailable verification service, invalid signature or missing attestation aborts
deployment. A registry mirror or supporting workload needs its own explicitly reviewed
trust entry and the same evidence, usually from an internally rebuilt/signed image.
GitHub CLI verification uses the exact `--cert-identity` and OIDC issuer from the
policy, plus repository, source-ref and hosted-runner constraints. It deliberately
does not combine that selector with `--signer-workflow` or `--signer-repo`, which
current GitHub CLI versions treat as mutually exclusive.
Do not broaden the signer identity to a wildcard to accommodate unsigned upstreams.

Use these wrappers for every install, upgrade and rollback to another digest. Direct
Helm/kubectl commands can bypass this client-side gate; restrict cluster deployment
credentials to the verified deployment path, or enforce equivalent Sigstore admission
policies cluster-wide. The source tree cannot attest a site's admission configuration.

## Actual repository settings

The `main` protection policy is versioned in `.github/main-protection.json`. It requires
PRs, passing CI from GitHub Actions against an up-to-date branch, and resolved
conversations. Independent approvals and approval after the last push are optional
for the current solo-maintainer workflow with AI assistance. Reviews remain available;
stale approvals are dismissed after changes. Administrators are subject to the
remaining protections. Force pushes and deletion are disabled. Required checks cover
Go/race/integration, browser/image, Helm, live network policy, deployment
verification/secret scanning, dependency review, and both CodeQL languages.

The `immutable-releases` tag ruleset in `.github/release-tags.json` covers `v*` tags
and forbids updates and deletion, with no bypass actors. Writers can create new tags
and dispatch the release workflow; only images matching the exact signing policy are
trusted by the deployment wrappers. Repository administrators can change rulesets,
and registry administrators can alter GHCR packages; the workflow's registry check
does not claim to make GHCR tags immutable against those administrators. Always
deploy the digest recorded in `image.txt`.

Independent release approval is optional. The release build/sign job uses the
`release` environment; the default `.github/release-approval.json` keeps the approved
solo-maintainer mode. To require independent approval, configure required reviewers
and prevent self-review on that environment, then set `independent_approval: true` in
the policy. `tests/security/repository_controls.py` verifies the selected mode, tag
protections and all PR bypass allowances. Technical checks remain mandatory in both
modes. Its `--apply` option applies main and tag protections, leaving environment
reviewers for the repository administrator to select.

Repository secret scanning, push protection, Dependabot security updates and mandatory
Action SHA pinning are enabled. CodeQL scans Go and JavaScript/TypeScript on PRs, main
pushes and weekly; dependency review rejects high/critical vulnerable additions.
Dependabot proposes weekly Go module, npm, root Dockerfile base-image and Action
updates. It does not update our Compose/Helm images or `go run`/`go install` tool
pins; the weekly watcher below covers those. CI runs Gitleaks with its default
provider/generic rules and redacted output, both against the checkout and every
commit added by a PR or push. An added-then-deleted secret therefore still fails
the history scan. Initial pushes without a base scan all reachable history. The
synthetic history regression test verifies this without printing its fixture.
GitHub currently reports
non-provider patterns and validity checks disabled; Gitleaks supplies the generic-secret
alternative. Its only allowlist matches the public `OIDC_AUDIENCE` name or its exact `=olr-api` example
assignment consumed after an empty example secret; it does not exempt a file or actual credential value.

### Pinned image and tool updates

`pinned-updates.yml` checks official upstream release records every Monday and
can also run manually. It opens one maintenance review issue for a new upstream
snapshot, updates that issue while open, and leaves reviewed snapshots closed.
Lookup failures fail the workflow instead of producing an incomplete report.
CI rejects any new digest-pinned image or Go tool without an inventoried update
source. The inventory is in `deploy/release/pinned_updates.py`:

| Pins | Update path |
| --- | --- |
| Root Dockerfile Node, Go, Debian and distroless images | Weekly Dependabot Docker PRs |
| Helm PostgreSQL and Keycloak | Weekly upstream tag/release review issue, including PostgreSQL maintenance branches |
| Compose Prometheus, Grafana and Jaeger | Weekly upstream release review issue |
| Optional digest-only vLLM CPU fixture | Weekly vLLM release review issue; manually select the corresponding CPU build and verify its digest |
| Gitsign, Gitleaks, go-licenses, staticcheck and govulncheck | Weekly upstream release/module-tag review issue |
| Site-provided router/custom image digests in deployment examples | Manually maintained by the site operator; verify each replacement with the deployment signature gate |

For each review, compare the listed upstream versions to the pins in its source
files. Update version and digest together from the official registry, verify
upstream provenance, and run the relevant image build, license, client,
Compose/Helm render and vulnerability checks. Major upgrades need compatibility
review. Record the accepted upgrade or reason for retaining a version in the
review issue, then close it. These proposals never auto-merge or deploy.
`python3 deploy/release/pinned_updates.py` prints a read-only report using the
GitHub CLI; publishing is limited to the scheduled workflow's issue permission.

Code-scanning alert [#1](https://github.com/arm2arm/open-llm-router/security/code-scanning/1)
classifies the load-test API key as a password. This is a false positive: `auth.Generate`
creates keys from 32 cryptographically random bytes, and issuance, authentication and
load-test accounting use the same SHA-256 lookup digest. Human passwords instead use
individually salted Argon2id (`internal/auth/password.go`). Applying PBKDF2 only to the
accounting lookup would make existing keys unfindable. `TestAccountingFindsIssuedAPIKey`
checks the lookup against a key issued and authenticated through the real store in an
isolated, migrated database. This triage applies only to that alert; the CodeQL rule
and both language scans remain enabled. The [CodeQL query guidance](https://codeql.github.com/codeql-query-help/go/go-weak-sensitive-data-hashing/)
distinguishes password KDFs from SHA-2/SHA-3 for other inputs.

Recheck the actual settings through the API (requires repository administration access):

```sh
python3 tests/security/repository_controls.py
# Authorized repository administrators can reconcile the versioned protection policy:
python3 tests/security/repository_controls.py --apply
```

These controls complement review and deployment-specific verification. An actual signed
release and production deployment require a maintainer dispatch and site infrastructure;
CI tests use synthetic images and deliberately failing verifiers to prove rejection.

References: [GitHub attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations),
[Cosign verification](https://docs.sigstore.dev/cosign/verifying/verify/),
[GitHub CLI verification policy](https://cli.github.com/manual/gh_attestation_verify),
[Gitsign verification](https://github.com/sigstore/gitsign#verification).
