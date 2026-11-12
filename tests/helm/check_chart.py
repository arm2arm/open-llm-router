"""Chart contracts: real Helm rendering with synthetic deployment values only."""

import base64
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from urllib.parse import urlsplit, unquote

import yaml


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "charts/open-llm-router"
HELM = os.environ.get("HELM", "helm")
NAME = "trial-open-llm-router"


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
        resources = [item for item in yaml.safe_load_all(result.stdout) if item]
        indexed = {(item["kind"], item["metadata"]["name"]): item for item in resources}
        self.assertEqual(len(indexed), len(resources), "duplicate resource identities")
        return indexed

    def test_default_stack_and_generated_credentials(self):
        resources = self.render()
        self.assertEqual(len(resources), 13)
        for kind, suffix in [("Deployment", ""), ("StatefulSet", "-postgresql"),
                             ("Deployment", "-keycloak"), ("Deployment", "-demo")]:
            self.assertIn((kind, NAME + suffix), resources)
        secret = resources["Secret", NAME + "-credentials"]
        data = {key: base64.b64decode(value).decode() for key, value in secret["data"].items()}
        self.assertGreaterEqual(len(data["OLR_BOOTSTRAP_ADMIN_PASSWORD"]), 15)
        self.assertEqual(len(base64.b64decode(data["OLR_API_KEY_ENCRYPTION_KEY"])), 32)
        url = urlsplit(data["OLR_DATABASE_URL"])
        self.assertEqual(url.hostname, NAME + "-postgresql")
        self.assertEqual(url.password, data["POSTGRES_PASSWORD"])
        self.assertEqual(secret["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        for suffix in ["-postgresql", "-keycloak"]:
            pvc = resources["PersistentVolumeClaim", NAME + suffix]
            self.assertEqual(pvc["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
            self.assertNotIn("storageClassName", pvc["spec"])

    def test_default_deployment_is_restricted_and_release_scoped(self):
        resources = self.render()
        deployment = resources["Deployment", NAME]
        self.assertEqual(deployment["spec"]["replicas"], 1)
        self.assertEqual(deployment["spec"]["strategy"]["type"], "Recreate")
        pod = deployment["spec"]["template"]["spec"]
        self.assertEqual(pod["securityContext"]["runAsUser"], 65532)
        self.assertGreaterEqual(pod["terminationGracePeriodSeconds"], 35)
        router = pod["containers"][0]
        self.assertEqual(router["envFrom"], [{"secretRef": {"name": NAME + "-credentials"}}])
        self.assertEqual(router["readinessProbe"]["httpGet"]["path"], "/readyz")
        self.assertEqual(router["livenessProbe"]["httpGet"]["path"], "/healthz")
        self.assertEqual(pod["initContainers"][0]["name"], "wait-for-postgresql")
        self.assertEqual(pod["volumes"][0]["configMap"]["name"], NAME + "-config")
        labels = deployment["spec"]["selector"]["matchLabels"]
        self.assertEqual(labels, resources["Service", NAME]["spec"]["selector"])
        self.assertEqual(labels["app.kubernetes.io/instance"], "trial")
        other = self.render(release="other")
        self.assertFalse(set(resources) & set(other))
        for (kind, name), item in resources.items():
            self.assertEqual(item["metadata"]["namespace"], "team")
            if kind not in ["Deployment", "StatefulSet"]:
                continue
            spec = item["spec"]["template"]["spec"]
            self.assertFalse(spec["automountServiceAccountToken"])
            self.assertTrue(spec["securityContext"]["runAsNonRoot"])
            for container in spec["containers"] + spec.get("initContainers", []):
                self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
                self.assertEqual(container["securityContext"]["capabilities"]["drop"], ["ALL"])
            # Every Service matches only its own workload.
            selector = resources["Service", name]["spec"]["selector"]
            matches = [other_name for (other_kind, other_name), other_item in resources.items()
                       if other_kind in ["Deployment", "StatefulSet"] and all(
                           other_item["spec"]["template"]["metadata"]["labels"].get(k) == v
                           for k, v in selector.items())]
            self.assertEqual(matches, [name])

    def test_sso_and_demo_connections(self):
        resources = self.render()
        env = {v["name"]: v["value"] for v in resources["Deployment", NAME]["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["OIDC_ISSUER"], "http://localhost:8081/realms/olr-demo")
        self.assertEqual(env["OIDC_BACKCHANNEL_URL"], f"http://{NAME}-keycloak:8080/realms/olr-demo")
        self.assertEqual(env["OLR_DEMO_BACKEND_URL"], f"http://{NAME}-demo:8000")
        realm = json.loads(resources["ConfigMap", NAME + "-keycloak-realm"]["data"]["olr-demo-realm.json"])
        self.assertEqual(realm["users"][0]["groups"], ["/staff"])
        self.assertEqual(realm["clients"][0]["secret"], "${OIDC_PORTAL_CLIENT_SECRET}")
        self.assertEqual(realm["clients"][0]["redirectUris"], ["${PORTAL_BASE_URL}/portal/callback"])

    def test_custom_credentials_and_storage(self):
        password = "db:pass/@?#% +value"
        values = {"postgresql": {"password": password, "persistence": {"storageClass": "fast", "size": "16Gi"}},
                  "secrets": {"bootstrapAdminPassword": "test-bootstrap-password", "apiKeyEncryptionKey": "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=", "env": {"GPU_TOKEN": "test-token"}},
                  "keycloak": {"clientSecret": "test-client-secret", "demoUserPassword": "test-staff-password"}}
        resources = self.render(values)
        data = {key: base64.b64decode(value).decode() for key, value in resources["Secret", NAME + "-credentials"]["data"].items()}
        self.assertEqual(data["POSTGRES_PASSWORD"], password)
        self.assertEqual(unquote(urlsplit(data["OLR_DATABASE_URL"]).password), password)
        self.assertEqual(data["GPU_TOKEN"], "test-token")
        self.assertEqual(data["OIDC_PORTAL_CLIENT_SECRET"], "test-client-secret")
        self.assertEqual(data["OLR_DEMO_USER_PASSWORD"], "test-staff-password")
        pvc = resources["PersistentVolumeClaim", NAME + "-postgresql"]["spec"]
        self.assertEqual(pvc["storageClassName"], "fast")
        self.assertEqual(pvc["resources"]["requests"]["storage"], "16Gi")
        ephemeral = self.render({"postgresql": {"persistence": {"enabled": False}}, "keycloak": {"persistence": {"enabled": False}}})
        self.assertFalse(any(kind == "PersistentVolumeClaim" for kind, _ in ephemeral))
        for kind, name in [("StatefulSet", NAME + "-postgresql"), ("Deployment", NAME + "-keycloak")]:
            volumes = ephemeral[kind, name]["spec"]["template"]["spec"]["volumes"]
            self.assertEqual(volumes[0], {"name": "data", "emptyDir": {}})

    def test_image_environment_and_config_changes(self):
        digest = "sha256:" + "a" * 64
        values = {"image": {"repository": "registry.invalid/router", "digest": digest},
                  "imagePullSecrets": [{"name": "registry-auth"}], "existingSecret": "site-secrets",
                  "keycloak": {"enabled": False}, "env": {"OIDC_ISSUER": "https://login.invalid/realms/team"},
                  "config": {"models": {"chat-model": {"upstream_model": "${SERVED_MODEL}"}}}}
        rendered = self.render(values)
        pod = rendered["Deployment", NAME]["spec"]["template"]["spec"]
        self.assertEqual(pod["containers"][0]["image"], "registry.invalid/router@" + digest)
        self.assertEqual(pod["imagePullSecrets"], [{"name": "registry-auth"}])
        self.assertEqual(pod["containers"][0]["envFrom"][0]["secretRef"]["name"], "site-secrets")
        self.assertFalse(any(kind == "Secret" for kind, _ in rendered))
        config = yaml.safe_load(rendered["ConfigMap", NAME + "-config"]["data"]["config.yaml"])
        self.assertEqual(config["models"]["chat-model"]["upstream_model"], "${SERVED_MODEL}")
        checksum = rendered["Deployment", NAME]["spec"]["template"]["metadata"]["annotations"]["checksum/config"]
        self.assertNotEqual(checksum, self.render()["Deployment", NAME]["spec"]["template"]["metadata"]["annotations"]["checksum/config"])

    def test_production_example_external_services(self):
        rendered = self.render(yaml.safe_load((CHART / "examples/production.yaml").read_text()))
        self.assertEqual(len(rendered), 4)
        ingress = rendered["Ingress", NAME]
        self.assertEqual(ingress["spec"]["tls"][0]["secretName"], "olr-tls")
        self.assertEqual(ingress["spec"]["rules"][0]["host"], "llm.example.org")
        self.assertEqual(ingress["metadata"]["annotations"]["nginx.org/proxy-buffering"], "False")
        self.assertEqual(ingress["metadata"]["annotations"]["nginx.org/proxy-read-timeout"], "960s")
        pod = rendered["Deployment", NAME]["spec"]["template"]["spec"]
        self.assertNotIn("initContainers", pod)
        self.assertIn({"name": "PORTAL_COOKIE_SECURE", "value": "true"}, pod["containers"][0]["env"])

    def test_external_database_with_chart_managed_secret(self):
        url = "postgres://user:pass@db.invalid/olr?sslmode=verify-full"
        resources = self.render({"postgresql": {"enabled": False}, "externalDatabase": {"url": url}})
        self.assertFalse(any(kind == "StatefulSet" for kind, _ in resources))
        self.assertEqual(base64.b64decode(resources["Secret", NAME + "-credentials"]["data"]["OLR_DATABASE_URL"]).decode(), url)

    def test_long_names_remain_valid(self):
        resources = self.render({"fullnameOverride": "x" * 63, "nameOverride": "y" * 63})
        for _, name in resources:
            self.assertLessEqual(len(name), 63)

    def test_invalid_values_fail_before_install(self):
        invalid = [{"replicaCount": 2}, {"postgresql": {"enabled": False}},
                   {"env": {"PORTAL_BASE_URL": "https://wrong.invalid"}},
                   {"env": {"OLR_TRACE_USER_ID": True}}, {"env": {"OIDC_ISSUER": "https://wrong.invalid"}},
                   {"env": {"OLR_DATABASE_URL": "postgres://wrong.invalid/olr"}},
                   {"secrets": {"bootstrapAdminPassword": "short"}},
                   {"secrets": {"apiKeyEncryptionKey": "invalid"}},
                   {"secrets": {"env": {"POSTGRES_PASSWORD": "collision"}}},
                   {"portal": {"cookieSecure": True}}, {"ingress": {"enabled": True}}]
        for values in invalid:
            with self.subTest(values=values), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "values.yaml"
                path.write_text(yaml.safe_dump(values))
                result = self.invoke("template", "trial", CHART, "-f", path)
                self.assertNotEqual(result.returncode, 0)

    def test_lint_examples_and_package(self):
        for values in [None, "demo.values.yaml", "production.yaml"]:
            args = ["lint", CHART, "--strict"]
            if values:
                args += ["-f", CHART / "examples" / values]
            lint = self.invoke(*args)
            self.assertEqual(lint.returncode, 0, lint.stdout + lint.stderr)
        with tempfile.TemporaryDirectory() as directory:
            package = self.invoke("package", CHART, "--destination", directory)
            self.assertEqual(package.returncode, 0, package.stderr)
            with tarfile.open(next(Path(directory).glob("*.tgz"))) as archive:
                for required in ("values.schema.json", "README.md", "examples/production.yaml", "examples/demo.values.yaml", "files/olr-demo-realm.json"):
                    self.assertIn("open-llm-router/" + required, archive.getnames())


if __name__ == "__main__":
    unittest.main()
