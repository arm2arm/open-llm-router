#!/usr/bin/env python3
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import uuid
from urllib.parse import parse_qsl, urlsplit

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("run_libpq", ROOT / "deploy/postgres/run_libpq.py")
libpq = importlib.util.module_from_spec(spec)
spec.loader.exec_module(libpq)


class LibpqURLTests(unittest.TestCase):
    def test_pool_options_are_removed_without_changing_credentials_or_tls(self):
        original = "postgres://user:synthetic%40password@db.example/olr?pool_max_conns=12&pool_min_conns=2&sslmode=verify-full&sslrootcert=%2Fprivate%2Fca.pem&application_name=backup&statement_cache_capacity=64"
        result = urlsplit(libpq.libpq_url(original))
        self.assertEqual(result.netloc, urlsplit(original).netloc)
        self.assertEqual(result.path, "/olr")
        self.assertEqual(dict(parse_qsl(result.query)), {"sslmode": "verify-full", "sslrootcert": "/private/ca.pem", "application_name": "backup"})

    def test_empty_parameters_and_postgresql_scheme_survive(self):
        self.assertEqual(libpq.libpq_url("postgresql:///olr?host=%2Fvar%2Frun%2Fpostgresql&password="), "postgresql:///olr?host=%2Fvar%2Frun%2Fpostgresql&password=")

    def test_non_postgres_url_is_refused(self):
        with self.assertRaises(ValueError):
            libpq.libpq_url("https://example.invalid")

    def test_password_moves_to_environment_without_losing_host_or_user(self):
        url, password = libpq.connection("postgres://user:synthetic%40password@[::1]:5432/olr?pool_max_conns=12&sslmode=verify-full")
        self.assertEqual(url, "postgres://user@[::1]:5432/olr?sslmode=verify-full")
        self.assertEqual(password, "synthetic@password")

    def test_query_password_overrides_userinfo_and_is_not_in_arguments(self):
        url, password = libpq.connection("postgres://user:old@one:5432,two:5433/olr?password=new%26value")
        self.assertEqual(url, "postgres://user@one:5432,two:5433/olr")
        self.assertEqual(password, "new&value")

    def test_libpq_uri_uses_percent_encoding_for_spaces_and_literal_plus(self):
        result = libpq.libpq_url("postgres://user@db/olr?sslrootcert=%2Fprivate%2Froot+ca%2B1.pem&pool_max_conns=8")
        self.assertEqual(result, "postgres://user@db/olr?sslrootcert=%2Fprivate%2Froot%20ca%2B1.pem")

    @unittest.skipUnless(os.environ.get("OLR_DOCS_POSTGRES_CONTAINER"), "set a disposable PostgreSQL container for the real dump/restore check")
    def test_documented_backup_and_restore_with_pool_options(self):
        container = os.environ["OLR_DOCS_POSTGRES_CONTAINER"]
        base = os.environ["OLR_TEST_DATABASE_URL"]
        suffix = uuid.uuid4().hex
        source, target = "olr_docs_source_" + suffix, "olr_docs_target_" + suffix
        with tempfile.TemporaryDirectory() as directory:
            # Execute the actual wrapper on the host, and only substitute the
            # client binaries with the clients in our disposable DB container.
            for command in ["psql", "pg_dump", "pg_restore"]:
                path = Path(directory) / command
                path.write_text(f"#!{sys.executable}\nimport os,sys\nos.execvp('docker', ['docker','exec','-i','--env','PGPASSWORD',{container!r},{command!r},*sys.argv[1:]])\n")
                path.chmod(0o755)
            env = dict(os.environ, PATH=directory + os.pathsep + os.environ["PATH"])
            def call(database, command, *args, data=None):
                parts = urlsplit(base)
                query = parts.query + "&pool_max_conns=7&pool_min_conns=1&application_name=backup+check%2Bdocs"
                env["OLR_DATABASE_URL"] = parts._replace(path="/" + database, query=query).geturl()
                result = subprocess.run([sys.executable, str(ROOT / "deploy/postgres/run_libpq.py"), command, *args], env=env, input=data, capture_output=True, timeout=30)
                self.assertEqual(result.returncode, 0, result.stderr.decode(errors="replace"))
                return result.stdout
            original = urlsplit(base).path.lstrip("/")
            try:
                self.assertEqual(call(original, "psql", "-Atc", "SELECT current_setting('application_name')").strip(), b"backup check+docs")
                for name in [source, target]:
                    call(original, "psql", "-c", "CREATE DATABASE " + name)
                call(source, "psql", "-c", "CREATE TABLE backup_example(value integer); INSERT INTO backup_example VALUES(42)")
                dump = call(source, "pg_dump", "--format=custom")
                self.assertTrue(dump.startswith(b"PGDMP"))
                call(target, "pg_restore", "--clean", "--if-exists", data=dump)
                self.assertEqual(call(target, "psql", "-Atc", "SELECT value FROM backup_example").strip(), b"42")
            finally:
                for name in [source, target]:
                    call(original, "psql", "-c", "DROP DATABASE IF EXISTS " + name)


if __name__ == "__main__":
    if os.environ.get("CI") == "true" and not os.environ.get("OLR_DOCS_POSTGRES_CONTAINER"):
        raise SystemExit("CI must set OLR_DOCS_POSTGRES_CONTAINER for the documented dump/restore check")
    unittest.main()
