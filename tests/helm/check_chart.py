"""Chart contracts: real Helm rendering with synthetic deployment values only."""

import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "charts/open-llm-router"
HELM = os.environ.get("HELM", "helm")


class HelmChart(unittest.TestCase):
    def invoke(self, *args):
        return subprocess.run(
            [HELM, *map(str, args)], text=True, capture_output=True, check=False,
        )

    def render(self, values=None, release="trial"):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "values.yaml"
            path.write_text(yaml.safe_dump(values or {}))
            result = self.invoke("template", release, CHART, "-n", "team", "-f", path)
        self.assertEqual(result.returncode, 0, result.stderr)
        return {item["kind"]: item for item in yaml.safe_load_all(result.stdout) if item}

    def test_default_deployment_is_restricted_and_release_scoped(self):
        resources = self.render()
        self.assertEqual(set(resources), {"Deployment", "ConfigMap", "Service"})
        deployment = resources["Deployment"]
        self.assertEqual(deployment["metadata"]["namespace"], "team")
        self.assertEqual(deployment["spec"]["replicas"], 1)
        self.assertEqual(deployment["spec"]["strategy"]["type"], "Recreate")
        pod = deployment["spec"]["template"]["spec"]
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertEqual(pod["securityContext"]["runAsUser"], 65532)
        self.assertGreaterEqual(pod["terminationGracePeriodSeconds"], 35)
        router = pod["containers"][0]
        self.assertEqual(router["envFrom"], [{"secretRef": {"name": "olr-secrets"}}])
        self.assertTrue(router["securityContext"]["readOnlyRootFilesystem"])
        self.assertFalse(router["securityContext"]["allowPrivilegeEscalation"])
        self.assertEqual(router["securityContext"]["capabilities"]["drop"], ["ALL"])
        self.assertEqual(router["readinessProbe"]["httpGet"]["path"], "/readyz")
        self.assertEqual(router["livenessProbe"]["httpGet"]["path"], "/healthz")
        labels = deployment["spec"]["selector"]["matchLabels"]
        self.assertEqual(labels, resources["Service"]["spec"]["selector"])
        self.assertEqual(labels["app.kubernetes.io/instance"], "trial")
        other = self.render(release="other")
        self.assertNotEqual(deployment["metadata"]["name"], other["Deployment"]["metadata"]["name"])
        self.assertNotEqual(labels, other["Service"]["spec"]["selector"])
        self.assertEqual(pod["volumes"][0]["configMap"]["name"], resources["ConfigMap"]["metadata"]["name"])

    def test_image_environment_and_config_changes(self):
        digest = "sha256:" + "a" * 64
        values = {
            "image": {"repository": "registry.invalid/router", "digest": digest},
            "imagePullSecrets": [{"name": "registry-auth"}],
            "existingSecret": "site-secrets",
            "env": {"OIDC_ISSUER": "https://login.invalid/realms/team"},
            "config": {"models": {"chat-model": {"upstream_model": "${SERVED_MODEL}"}}},
        }
        rendered = self.render(values)
        pod = rendered["Deployment"]["spec"]["template"]["spec"]
        router = pod["containers"][0]
        self.assertEqual(router["image"], "registry.invalid/router@" + digest)
        self.assertEqual(pod["imagePullSecrets"], [{"name": "registry-auth"}])
        self.assertEqual(router["envFrom"][0]["secretRef"]["name"], "site-secrets")
        environment = {item["name"]: item["value"] for item in router["env"]}
        self.assertEqual(environment["PORTAL_COOKIE_SECURE"], "false")
        self.assertEqual(environment["OIDC_ISSUER"], values["env"]["OIDC_ISSUER"])
        config = yaml.safe_load(rendered["ConfigMap"]["data"]["config.yaml"])
        self.assertEqual(config["models"]["chat-model"]["upstream_model"], "${SERVED_MODEL}")
        checksum = rendered["Deployment"]["spec"]["template"]["metadata"]["annotations"]["checksum/config"]
        default_checksum = self.render()["Deployment"]["spec"]["template"]["metadata"]["annotations"]["checksum/config"]
        self.assertNotEqual(checksum, default_checksum)

    def test_production_example_tls_and_streaming(self):
        rendered = self.render(yaml.safe_load((CHART / "examples/production.yaml").read_text()))
        ingress = rendered["Ingress"]
        self.assertEqual(ingress["spec"]["tls"][0]["secretName"], "olr-tls")
        self.assertEqual(ingress["spec"]["rules"][0]["host"], "llm.example.org")
        self.assertEqual(ingress["metadata"]["annotations"]["nginx.org/proxy-buffering"], "False")
        self.assertEqual(ingress["metadata"]["annotations"]["nginx.org/proxy-read-timeout"], "960s")
        backend = ingress["spec"]["rules"][0]["http"]["paths"][0]["backend"]["service"]
        self.assertEqual(backend["name"], rendered["Service"]["metadata"]["name"])
        environment = rendered["Deployment"]["spec"]["template"]["spec"]["containers"][0]["env"]
        self.assertIn({"name": "PORTAL_COOKIE_SECURE", "value": "true"}, environment)

    def test_invalid_values_fail_before_install(self):
        invalid = [
            {"replicaCount": 2},
            {"existingSecret": ""},
            {"env": {"PORTAL_BASE_URL": "https://wrong.invalid"}},
            {"env": {"OLR_TRACE_USER_ID": True}},
            {"portal": {"cookieSecure": True}},
            {"ingress": {"enabled": True}},
        ]
        for values in invalid:
            with self.subTest(values=values), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "values.yaml"
                path.write_text(yaml.safe_dump(values))
                result = self.invoke("template", "trial", CHART, "-f", path)
                self.assertNotEqual(result.returncode, 0)

    def test_lint_and_package(self):
        lint = self.invoke("lint", CHART, "--strict")
        self.assertEqual(lint.returncode, 0, lint.stdout + lint.stderr)
        with tempfile.TemporaryDirectory() as directory:
            package = self.invoke("package", CHART, "--destination", directory)
            self.assertEqual(package.returncode, 0, package.stderr)
            with tarfile.open(next(Path(directory).glob("*.tgz"))) as archive:
                paths = archive.getnames()
                for required in ("values.schema.json", "README.md", "examples/production.yaml"):
                    self.assertIn("open-llm-router/" + required, paths)


if __name__ == "__main__":
    unittest.main()
