# Contributing

Read [the security policy](SECURITY.md) before reporting a security problem.
For an ordinary bug, include the version/commit, steps to reproduce and expected
behavior with synthetic data. Never attach `.env`, credentials or personal traces.

Start a focused branch from current `main`. Explain the observable problem and
resulting behavior in the PR, update affected operator/client documentation, and
add a regression test at the actual boundary. Use existing fixtures and keep
tests deterministic. Follow the repository's Go formatting and frontend linting.

Run these checks from the repository root:

```sh
make lint
make vuln
make web-check
```

Integration tests need a disposable PostgreSQL 17 instance and a role that can
create/drop databases. Never point them at a deployed router database:

```sh
export OLR_TEST_DATABASE_URL='postgres://olr:test-only@127.0.0.1:5432/olr_test?sslmode=disable'
export OLR_REQUIRE_DATABASE=1
go test -race ./...
python3 tests/security/check_mutations.py
```

The DSN's database must exist. Tests create isolated databases and clean them up.
Without the setting, database suites skip only during ordinary local runs;
`CI=true` or `OLR_REQUIRE_DATABASE=1` makes a missing setting fail. See
[test setup and fuzzing](docs/REFERENCE.md#tests) for SDK/MCP Python fixtures and
the [Makefile](Makefile) for image, Compose and browser checks.

All required GitHub checks must pass before merge. Their source of truth is
[the main-branch policy](.github/main-protection.json); workflow steps, supported
tool versions and timeouts are in [CI](.github/workflows/ci.yml) and
[CodeQL](.github/workflows/codeql.yml). This includes database/race tests,
security mutations, frontend/browser and client behavior, image/license checks,
Helm/network isolation, secret history and release policy. Do not bypass a failed
check or mark an untested change complete.

Record user-visible changes and migration requirements in [CHANGELOG.md](CHANGELOG.md).
Release maintainers follow [the signed release procedure](docs/RELEASE_SECURITY.md).
