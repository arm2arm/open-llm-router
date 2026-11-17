# Security policy

Report suspected vulnerabilities privately through
[GitHub's vulnerability report form](https://github.com/arm2arm/open-llm-router/security/advisories/new).
Private vulnerability reporting is enabled. Do not put exploit details, credentials,
personal request content or a live installation's secrets in a public issue.

Include the affected commit/release, configuration preconditions, expected and
observed behavior, impact, and a minimal reproduction using synthetic data. Keep
production endpoints and identifying data out of attachments unless needed for
the private investigation. Never send a live API key or password.

The maintainer aims to acknowledge a private report within seven calendar days
and provide a triage update within fourteen days. Continue discussion in the
private advisory; coordinated disclosure and a patched release follow validation.

Security fixes target `main` and the newest published release. Earlier releases
do not have a maintained security backport branch; upgrade to the latest fixed
release and review [upgrade notes](CHANGELOG.md). This project is in pilot use;
an audit or passing CI does not replace the site's deployment acceptance.

Use public issues for non-sensitive bugs and feature requests. The
[release and repository controls](docs/RELEASE_SECURITY.md) document signed images,
dependency updates, secret scanning and the limits of these controls.
