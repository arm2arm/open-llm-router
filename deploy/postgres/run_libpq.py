#!/usr/bin/env python3
"""Run pg_dump, pg_restore or psql with a router URL adapted for libpq.

Read OLR_DATABASE_URL from the environment. Pass its database password through
PGPASSWORD, remove pgx-only options, and preserve TLS/service options.
"""
import os
import sys
from urllib.parse import parse_qsl, quote, unquote, urlencode, urlsplit

PGX_ONLY = {"statement_cache_capacity", "description_cache_capacity", "default_query_exec_mode"}


def libpq_url(raw):
    parts = urlsplit(raw)
    if parts.scheme not in {"postgres", "postgresql"}:
        raise ValueError("OLR_DATABASE_URL must be a PostgreSQL URL")
    query = [(key, value) for key, value in parse_qsl(parts.query, keep_blank_values=True)
             if not key.startswith("pool_") and key not in PGX_ONLY]
    if parts.fragment:
        raise ValueError("database URL fragments are unsupported")
    suffix = "?" + urlencode(query, quote_via=quote) if query else ""
    return parts.scheme + "://" + parts.netloc + parts.path + suffix


def connection(raw):
    parts = urlsplit(libpq_url(raw))
    authority, password = parts.netloc, None
    if "@" in authority:
        user, host = authority.rsplit("@", 1)
        if ":" in user:
            user, encoded = user.split(":", 1)
            password = unquote(encoded)
        authority = user + "@" + host
    query = []
    for key, value in parse_qsl(parts.query, keep_blank_values=True):
        if key == "password":
            password = value
        else:
            query.append((key, value))
    suffix = "?" + urlencode(query, quote_via=quote) if query else ""
    return parts.scheme + "://" + authority + parts.path + suffix, password


def main():
    if len(sys.argv) < 2 or sys.argv[1] not in {"pg_dump", "pg_restore", "psql"}:
        raise SystemExit("usage: run_libpq.py pg_dump|pg_restore|psql [arguments]")
    try:
        database, password = connection(os.environ["OLR_DATABASE_URL"])
    except (KeyError, ValueError):
        raise SystemExit("set OLR_DATABASE_URL to a valid PostgreSQL URL") from None
    if any(arg == "-d" or arg.startswith("--dbname") for arg in sys.argv[2:]):
        raise SystemExit("database is selected by OLR_DATABASE_URL; omit -d/--dbname")
    environment = dict(os.environ)
    if password is not None:
        environment["PGPASSWORD"] = password
    os.execvpe(sys.argv[1], [sys.argv[1], "--dbname=" + database, *sys.argv[2:]], environment)


if __name__ == "__main__":
    main()
