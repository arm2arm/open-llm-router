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

The trusted certificate identity is the exact release workflow on `refs/heads/main`,
with issuer `https://token.actions.githubusercontent.com`. Signing uses the runner's
short-lived OIDC identity. No long-lived signing key is stored. No release is created
by merging this workflow; maintainers choose the version and dispatch it separately.
A failed publication may leave a signed image or tag. Investigate it before retrying;
the workflow refuses to replace existing tags. Do not move a published release tag.

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

The Helm wrapper forces `production=true`. Chart schema rejects a missing or invalid
router digest, mutable enabled supporting images, disabled network policy and
disabled verified TLS. Bundled demo services are incompatible with production TLS;
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
Do not broaden the signer identity to a wildcard to accommodate unsigned upstreams.

Use these wrappers for every install, upgrade and rollback to another digest. Direct
Helm/kubectl commands can bypass this client-side gate; restrict cluster deployment
credentials to the verified deployment path, or enforce equivalent Sigstore admission
policies cluster-wide. The source tree cannot attest a site's admission configuration.

## Actual repository settings

The `main` protection policy is versioned in `.github/main-protection.json`. It requires
PRs, one independent maintainer approval, dismissal of stale approvals, approval after
the last push, passing CI from GitHub Actions against an up-to-date branch, and resolved
conversations. Administrators are subject to these rules. Force pushes and deletion are
disabled. Required checks cover Go/race/integration, browser/image, Helm, live network
policy, deployment verification/secret scanning, dependency review, and both CodeQL
languages. These stricter review settings are applied after the remediation PR merges;
the remediation itself uses the pre-existing protection policy.

Repository secret scanning, push protection, Dependabot security updates and mandatory
Action SHA pinning are enabled. CodeQL scans Go and JavaScript/TypeScript on PRs, main
pushes and weekly; dependency review rejects high/critical vulnerable additions.
Dependabot proposes weekly Go, npm, Docker and Action updates. CI also runs Gitleaks
with its default provider/generic rules and redacted output. GitHub currently reports
non-provider patterns and validity checks disabled; Gitleaks supplies the generic-secret
alternative. Its only allowlist matches a specific public variable name consumed after
an empty example secret; it does not exempt a file or actual credential value.

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
