"""Validate the production Compose contract using only synthetic site values."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import shutil
import shlex


ROOT = Path(__file__).resolve().parents[2]
COMPOSE = ROOT / "deploy/compose/pilot.yml"
REQUIRED = (
    "OLR_IMAGE", "OLR_SITE_CONFIG", "OLR_DATABASE_URL", "OIDC_ISSUER",
    "OIDC_PORTAL_CLIENT_SECRET", "OIDC_SYNC_CLIENT_SECRET", "PORTAL_BASE_URL",
    "OLR_METRICS_TOKEN",
)


class LocalCompose(unittest.TestCase):
    def test_documented_commands_preserve_root_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            shutil.copytree(ROOT / "deploy/compose", root / "deploy/compose")
            shutil.copy(ROOT / "compose.yaml", root)
            shutil.copy(ROOT / "Makefile", root)
            values = {
                "OLR_BOOTSTRAP_ADMIN_PASSWORD": "synthetic-test-password",
                "OLR_PORT": "18089",
                "OLR_DATABASE_URL": "postgres://test:synthetic@database.invalid/olr?sslmode=disable",
                "OLR_SETTINGS_ENCRYPTION_KEY": "synthetic-encryption-key",
                "OLR_CONFIG_FILE": "../../config.ollama.example.yaml",
                "OLLAMA_BASE_URL": "http://model.invalid:11434",
                "OLLAMA_MODEL": "test-model",
                "OLR_METRICS_TOKEN": "synthetic-custom-metrics",
            }
            (root / ".env").write_text("".join(f"{k}={v}\n" for k, v in values.items()))
            env = {k: v for k, v in os.environ.items()
                   if not k.startswith(("OLR_", "OIDC_", "PORTAL_", "OLLAMA_", "COMPOSE_"))}
            local = next(line[2:] for line in (ROOT / ".env.example").read_text().splitlines()
                         if line.startswith("# docker compose "))
            observability = subprocess.check_output(
                ["make", "--no-print-directory", "-n", "observability-up"], cwd=root, env=env, text=True)
            for command in (local, observability):
                args = shlex.split(command)
                args = args[:args.index("up")] + ["config", "--format", "json"]
                config = json.loads(subprocess.check_output(args, cwd=root, env=env, text=True))
                router = config["services"]["router"]
                for key, value in values.items():
                    if key not in ("OLR_PORT", "OLR_CONFIG_FILE", "OLR_METRICS_TOKEN"):
                        self.assertEqual(router["environment"][key], value)
                self.assertEqual(router["ports"][0]["published"], "18089")
                if "prometheus" in config["services"]:
                    self.assertTrue(router["volumes"][0]["source"].endswith("config.ollama.example.yaml"))
                    prometheus = config["services"]["prometheus"]
                    self.assertEqual(router["environment"]["OLR_METRICS_TOKEN"], values["OLR_METRICS_TOKEN"])
                    self.assertEqual(prometheus["environment"]["OLR_METRICS_TOKEN"], values["OLR_METRICS_TOKEN"])
                    self.assertIn('"$$OLR_METRICS_TOKEN"', prometheus["command"][0])


class PilotCompose(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.env_file = Path(self.directory.name) / "pilot.env"
        self.config_file = Path(self.directory.name) / "config.yaml"
        self.config_file.write_text(
            (ROOT / "deploy/compose/pilot.config.example.yaml").read_text()
        )
        self.values = dict(
            line.split("=", 1)
            for line in (ROOT / "deploy/compose/pilot.env.example").read_text().splitlines()
            if line and not line.startswith("#")
        )
        self.values.update({
            "OLR_IMAGE": "example.invalid/olr@sha256:" + "a" * 64,
            "OLR_SITE_CONFIG": str(self.config_file),
            "OLR_DATABASE_URL": "postgres://test:synthetic@db.invalid/olr?sslmode=verify-full",
            "OIDC_ISSUER": "https://login.invalid/realms/pilot",
            "OIDC_PORTAL_CLIENT_SECRET": "synthetic-portal",
            "OIDC_SYNC_CLIENT_SECRET": "synthetic-sync",
            "PORTAL_BASE_URL": "https://router.invalid",
            "OLR_METRICS_TOKEN": "synthetic-metrics",
            "OLR_UPSTREAM_MODEL": "served-model",
            "OLR_BACKEND_A_URL": "https://gpu-a.invalid",
            "OLR_BACKEND_B_URL": "https://gpu-b.invalid",
            "VLLM_A_API_KEY": "synthetic$a#with spaces",
            "VLLM_B_API_KEY": "synthetic-b",
            "CUSTOM_BACKEND_TOKEN": "custom$token",
        })

    def render(self, values):
        self.env_file.write_text("".join(f"{key}='{value}'\n" for key, value in values.items()))
        # Never inherit private site/development values from the caller.
        env = {
            key: value for key, value in os.environ.items()
            if not key.startswith(("OLR_", "OIDC_", "PORTAL_", "VLLM_", "COMPOSE_"))
        }
        env["OLR_PILOT_ENV_FILE"] = str(self.env_file)
        return subprocess.run(
            ["docker", "compose", "-f", str(COMPOSE), "--env-file", str(self.env_file),
             "config", "--format", "json"],
            cwd=self.directory.name, env=env, text=True, capture_output=True, check=False,
        )

    def test_production_service_and_environment(self):
        # Dev values in the site file must not weaken the production invariants.
        self.values.update(PORTAL_COOKIE_SECURE="false", OIDC_BACKCHANNEL_URL="http://keycloak:8080")
        result = self.render(self.values)
        self.assertEqual(result.returncode, 0, result.stderr)
        config = json.loads(result.stdout)
        self.assertEqual(set(config["services"]), {"router"})
        router = config["services"]["router"]
        self.assertNotIn("build", router)
        self.assertNotIn("depends_on", router)
        self.assertEqual(router["image"], self.values["OLR_IMAGE"])
        self.assertTrue(router["read_only"])
        self.assertEqual(router["user"], "65532:65532")
        self.assertEqual(router["cap_drop"], ["ALL"])
        self.assertIn("no-new-privileges:true", router["security_opt"])
        self.assertEqual(router["ports"][0]["host_ip"], "127.0.0.1")
        mount = router["volumes"][0]
        self.assertEqual(mount["source"], str(self.config_file))
        self.assertTrue(mount["read_only"])
        # Some Compose releases omit false fields from rendered JSON.
        self.assertFalse(mount.get("bind", {}).get("create_host_path", False))
        self.assertEqual(router["environment"]["PORTAL_COOKIE_SECURE"], "true")
        self.assertEqual(router["environment"]["OIDC_BACKCHANNEL_URL"], "")
        self.assertEqual(router["environment"]["OLR_CONFIG"], "/etc/olr/config.yaml")
        for key in ("VLLM_A_API_KEY", "CUSTOM_BACKEND_TOKEN", "OLR_BACKEND_A_URL", "OLR_USAGE_DAYS"):
            # Releases differ on escaping dollars in their JSON representation.
            self.assertIn(router["environment"][key], (self.values[key], self.values[key].replace("$", "$$")))

    def test_required_values_cannot_be_empty(self):
        for key in REQUIRED:
            with self.subTest(key=key):
                result = self.render({**self.values, key: ""})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(key, result.stderr)

    def test_required_claim_is_not_invented_and_audience_can_be_disabled(self):
        for values, expected in (
            (self.values, None),
            ({**self.values, "OIDC_REQUIRED_CLAIM": ""}, ""),
            ({**self.values, "OIDC_REQUIRED_CLAIM": "affiliation=member"}, "affiliation=member"),
            ({**self.values, "OIDC_REQUIRED_CLAIM": "none"}, "none"),
        ):
            with self.subTest(claim=expected):
                result = self.render(values)
                self.assertEqual(result.returncode, 0, result.stderr)
                env = json.loads(result.stdout)["services"]["router"]["environment"]
                self.assertEqual(env.get("OIDC_REQUIRED_CLAIM"), expected)
                if expected is None:
                    self.assertNotIn("OIDC_REQUIRED_CLAIM", env)
        for value in ("", "custom-audience"):
            result = self.render({**self.values, "OIDC_AUDIENCE": value})
            self.assertEqual(result.returncode, 0, result.stderr)
            env = json.loads(result.stdout)["services"]["router"]["environment"]
            self.assertEqual(env["OIDC_AUDIENCE"], value)

    def test_production_template_enforces_verified_tls(self):
        self.assertIn("security:\n  require_tls: true\n", self.config_file.read_text())
        result = self.render(self.values)
        self.assertEqual(result.returncode, 0, result.stderr)
        env = json.loads(result.stdout)["services"]["router"]["environment"]
        self.assertIn("sslmode=verify-full", env["OLR_DATABASE_URL"])


if __name__ == "__main__":
    unittest.main()
