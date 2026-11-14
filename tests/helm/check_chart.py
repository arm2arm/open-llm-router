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
    def test_agent_administration_opt_in_and_proxy_configuration(self):
        baseline = self.render()
        config = yaml.safe_load(baseline["ConfigMap", NAME + "-config"]["data"]["config.yaml"])
        self.assertFalse(config["admin_api"]["enabled"])
        self.assertEqual(config["admin_api"]["trusted_proxy_cidrs"], [])
        policy = {"enabled": True, "trusted_proxy_cidrs": ["10.0.0.0/24", "2001:db8::/64"],
                  "default_lifetime_days": 3, "max_lifetime_days": 30,
                  "max_active_keys": 5, "requests_per_minute": 20, "max_concurrent": 2}
        rendered = self.render({"config": {"admin_api": policy}})
        config = yaml.safe_load(rendered["ConfigMap", NAME + "-config"]["data"]["config.yaml"])
        self.assertEqual(config["admin_api"], policy)
        def checksum(resources):
            return resources["Deployment", NAME]["spec"]["template"]["metadata"]["annotations"]["checksum/config"]
        self.assertNotEqual(checksum(baseline), checksum(rendered))

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
        self.assertEqual(len(resources), 18)
        for kind, suffix in [("Deployment", ""), ("StatefulSet", "-postgresql"),
                             ("Deployment", "-keycloak"), ("Deployment", "-demo")]:
            self.assertIn((kind, NAME + suffix), resources)
        secret = resources["Secret", NAME + "-credentials"]
        data = {key: base64.b64decode(value).decode() for key, value in secret["data"].items()}
        self.assertGreaterEqual(len(data["OLR_BOOTSTRAP_ADMIN_PASSWORD"]), 15)
        self.assertNotIn("OLR_API_KEY_ENCRYPTION_KEY", data)
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

    def test_demo_can_be_disabled_without_environment_or_models(self):
        rendered = self.render({"demoBackend": {"enabled": False}})
        self.assertNotIn(("Deployment", NAME + "-demo"), rendered)
        config = yaml.safe_load(rendered["ConfigMap", NAME + "-config"]["data"]["config.yaml"])
        self.assertEqual(config["models"], {})
        self.assertFalse(config["security"]["require_tls"])
        self.assertNotIn("OLR_DEMO_BACKEND_URL", json.dumps(config))
        custom = {"demoBackend": {"enabled": False}, "config": {"models": {"real-model": {
            "protocols": ["openai"], "timeout_s": 30,
            "pool": [{"url": "http://inference:8000", "weight": 1, "max_inflight": 1}],
        }}}}
        config = yaml.safe_load(self.render(custom)["ConfigMap", NAME + "-config"]["data"]["config.yaml"])
        self.assertEqual(list(config["models"]), ["real-model"])
        # Values copied from chart 0.2 retain the old default pool explicitly.
        legacy = {"demoBackend": {"enabled": False}, "config": {"models": {"chat-model": {
            "pool": [{"url": "${OLR_DEMO_BACKEND_URL}", "weight": 2, "max_inflight": 3}],
        }}}}
        config = yaml.safe_load(self.render(legacy)["ConfigMap", NAME + "-config"]["data"]["config.yaml"])
        self.assertEqual(config["models"], {})
        legacy["demoBackend"]["enabled"] = True
        config = yaml.safe_load(self.render(legacy)["ConfigMap", NAME + "-config"]["data"]["config.yaml"])
        replica = config["models"]["chat-model"]["pool"][0]
        self.assertEqual(replica, {"url": f"http://{NAME}-demo:8000", "weight": 2, "max_inflight": 3})

    def test_traefik_keycloak_ingress_and_controller_peers(self):
        values = yaml.safe_load((CHART / "examples/traefik.values.yaml").read_text())
        rendered = self.render(values)
        for suffix, host, secret in [("", "llm.example.org", "olr-tls"),
                                     ("-keycloak", "login.example.org", "keycloak-tls")]:
            ingress = rendered["Ingress", NAME + suffix]["spec"]
            self.assertEqual(ingress["ingressClassName"], "traefik")
            self.assertEqual(ingress["tls"], [{"hosts": [host], "secretName": secret}])
            self.assertEqual(ingress["rules"][0]["host"], host)
            backend = ingress["rules"][0]["http"]["paths"][0]["backend"]["service"]
            self.assertEqual(backend, {"name": NAME + suffix, "port": {"name": "http"}})
        env = {e["name"]: e.get("value") for e in rendered["Deployment", NAME + "-keycloak"]["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["KC_HOSTNAME"], "https://login.example.org")
        self.assertEqual(env["KC_PROXY_HEADERS"], "xforwarded")
        router_env = {e["name"]: e.get("value") for e in rendered["Deployment", NAME]["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(router_env["OIDC_ISSUER"], "https://login.example.org/realms/olr-demo")
        self.assertEqual(router_env["OIDC_BACKCHANNEL_URL"], f"http://{NAME}-keycloak:8080/realms/olr-demo")
        router = rendered["NetworkPolicy", NAME + "-router"]["spec"]["ingress"][0]["from"]
        self.assertEqual(router, values["networkPolicy"]["ingressPeers"] + values["networkPolicy"]["monitoringPeers"])
        keycloak = rendered["NetworkPolicy", NAME + "-keycloak"]["spec"]["ingress"][1]["from"]
        self.assertEqual(keycloak, values["networkPolicy"]["ingressPeers"])
        default_peers = self.render()["NetworkPolicy", NAME + "-router"]["spec"]["ingress"][0]["from"]
        self.assertEqual(len(default_peers), 1)
        self.assertEqual(default_peers[0]["namespaceSelector"]["matchLabels"],
                         {"kubernetes.io/metadata.name": "monitoring"})
        # Controllers can publish nonstandard ports and provide a default TLS
        # certificate without a per-Ingress Secret.
        values["portal"]["baseURL"] = "https://llm.example.org:8443"
        values["keycloak"]["baseURL"] = "https://login.example.org:8443"
        values["ingress"]["tlsSecretName"] = ""
        values["keycloak"]["ingress"]["tlsSecretName"] = ""
        custom = self.render(values)
        self.assertNotIn("tls", custom["Ingress", NAME]["spec"])
        self.assertNotIn("tls", custom["Ingress", NAME + "-keycloak"]["spec"])

    def test_custom_credentials_and_storage(self):
        password = "db:pass/@?#% +value"
        values = {"postgresql": {"password": password, "persistence": {"storageClass": "fast", "size": "16Gi"}},
                  "secrets": {"bootstrapAdminPassword": "test-bootstrap-password", "env": {"GPU_TOKEN": "test-token"}},
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
        production = yaml.safe_load((CHART / "examples/production.yaml").read_text())
        production["image"]["digest"] = "sha256:" + "a" * 64
        rendered = self.render(production)
        self.assertEqual(len(rendered), 6)
        ingress = rendered["Ingress", NAME]
        self.assertEqual(ingress["spec"]["tls"][0]["secretName"], "olr-tls")
        self.assertEqual(ingress["spec"]["rules"][0]["host"], "llm.example.org")
        self.assertEqual(ingress["metadata"]["annotations"]["nginx.org/proxy-buffering"], "False")
        self.assertEqual(ingress["metadata"]["annotations"]["nginx.org/proxy-read-timeout"], "960s")
        annotations = ingress["metadata"]["annotations"]
        self.assertEqual(annotations["nginx.org/client-max-body-size"], "4m")
        self.assertEqual(annotations["nginx.org/limit-req-rate"], "10r/s")
        self.assertEqual(annotations["nginx.org/limit-req-key"], "${binary_remote_addr}")
        controller = yaml.safe_load((ROOT / "deploy/nginx/f5-controller-config.yaml").read_text())["data"]
        self.assertEqual(controller["client-max-body-size"], "4m")
        self.assertIn("limit_conn olr_clients 20;", controller["server-snippets"])
        self.assertIn("client_body_timeout 120s;", controller["server-snippets"])
        self.assertIn("proxy_request_buffering off;", controller["location-snippets"])

        pod = rendered["Deployment", NAME]["spec"]["template"]["spec"]
        self.assertNotIn("initContainers", pod)
        self.assertIn({"name": "PORTAL_COOKIE_SECURE", "value": "true"}, pod["containers"][0]["env"])

    def test_optional_digest_policy_rejects_mutable_images_when_enabled(self):
        import copy
        production = yaml.safe_load((CHART / "examples/production.yaml").read_text())
        production["enforceImageDigests"] = True
        production["image"]["digest"] = "sha256:" + "a" * 64
        invalid = []
        for digest in ("", "sha256:short", "latest"):
            values = copy.deepcopy(production)
            values["image"]["digest"] = digest
            invalid.append(values)
        for service in ("postgresql", "keycloak", "demoBackend"):
            values = copy.deepcopy(production)
            values[service] = {"enabled": True, "image": "mutable:latest" if service != "demoBackend" else {"digest": ""}}
            invalid.append(values)
        values = copy.deepcopy(production)
        values["networkPolicy"]["enabled"] = False
        invalid.append(values)
        for values in invalid:
            with self.subTest(values=values), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "values.yaml"
                path.write_text(yaml.safe_dump(values))
                result = self.invoke("template", "trial", CHART, "-f", path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("schema", result.stderr)

    def test_production_can_use_tags_and_optional_transport_tls(self):
        values = yaml.safe_load((CHART / "examples/production.yaml").read_text())
        values["enforceImageDigests"] = False
        values["image"].pop("digest")
        values["config"]["security"]["require_tls"] = False
        rendered = self.render(values)
        image = rendered["Deployment", NAME]["spec"]["template"]["spec"]["containers"][0]["image"]
        self.assertEqual(image, "registry.example.org/olr:REPLACE_WITH_TESTED_GIT_SHA")
        config = yaml.safe_load(rendered["ConfigMap", NAME + "-config"]["data"]["config.yaml"])
        self.assertFalse(config["security"]["require_tls"])

    def test_invalid_keycloak_ingress_and_missing_peers_fail(self):
        import copy
        valid = yaml.safe_load((CHART / "examples/traefik.values.yaml").read_text())
        invalid = []
        for key, value in [("enabled", False), ("baseURL", "https://wrong.example.org"),
                           ("baseURL", "http://login.example.org")]:
            values = copy.deepcopy(valid)
            values["keycloak"][key] = value
            invalid.append(values)
        values = copy.deepcopy(valid)
        values["networkPolicy"]["ingressPeers"] = []
        invalid.append(values)
        values = copy.deepcopy(valid)
        values["portal"]["baseURL"] = "https://wrong.example.org"
        invalid.append(values)
        for key in ("className", "host"):
            values = copy.deepcopy(valid)
            values["keycloak"]["ingress"][key] = ""
            invalid.append(values)
        for values in invalid:
            with self.subTest(values=values), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "values.yaml"
                path.write_text(yaml.safe_dump(values))
                self.assertNotEqual(self.invoke("template", "trial", CHART, "-f", path).returncode, 0)

    def test_bundled_and_demo_image_digests_render_in_all_containers(self):
        digest = "sha256:" + "b" * 64
        resources = self.render({"demoBackend": {"image": {"digest": digest}}})
        router = resources["Deployment", NAME]["spec"]["template"]["spec"]
        self.assertRegex(router["initContainers"][0]["image"], r"@sha256:[a-f0-9]{64}$")
        for kind, suffix in (("StatefulSet", "-postgresql"), ("Deployment", "-keycloak"), ("Deployment", "-demo")):
            image = resources[kind, NAME + suffix]["spec"]["template"]["spec"]["containers"][0]["image"]
            self.assertRegex(image, r"@sha256:[a-f0-9]{64}$")

    def test_external_database_with_chart_managed_secret(self):
        url = "postgres://user:pass@db.invalid/olr?sslmode=verify-full"
        resources = self.render({"postgresql": {"enabled": False}, "externalDatabase": {"url": url}})
        self.assertFalse(any(kind == "StatefulSet" for kind, _ in resources))
        self.assertEqual(base64.b64decode(resources["Secret", NAME + "-credentials"]["data"]["OLR_DATABASE_URL"]).decode(), url)

    def test_long_names_remain_valid(self):
        resources = self.render({"fullnameOverride": "x" * 63, "nameOverride": "y" * 63})
        for _, name in resources:
            self.assertLessEqual(len(name), 63)

    def test_long_release_names_are_isolated(self):
        first = self.render(release="r" * 52 + "a")
        second = self.render(release="r" * 52 + "b")
        self.assertFalse(set(first) & set(second))

    def test_network_policy_restricts_release_and_dependencies(self):
        resources = self.render()
        deny = resources["NetworkPolicy", NAME + "-deny"]["spec"]
        self.assertEqual(deny["policyTypes"], ["Ingress", "Egress"])
        self.assertEqual(deny["ingress"], [])
        self.assertEqual(deny["egress"], [])
        router = resources["NetworkPolicy", NAME + "-router"]["spec"]
        dns = router["egress"][0]
        self.assertEqual(dns["ports"], [{"port": 53, "protocol": "UDP"}, {"port": 53, "protocol": "TCP"}])
        for peer in router["ingress"][0]["from"]:
            self.assertIn("namespaceSelector", peer)
            self.assertIn("podSelector", peer)
        for suffix in ["-postgresql", "-demo"]:
            policy = resources["NetworkPolicy", NAME + suffix]["spec"]
            self.assertEqual(policy["egress"], [])
            self.assertEqual(policy["ingress"][0]["from"][0]["podSelector"]["matchLabels"]["app.kubernetes.io/name"], "open-llm-router")

    def test_unrestricted_router_egress_preserves_other_policy_boundaries(self):
        overlay = yaml.safe_load((CHART / "examples/unrestricted-egress.values.yaml").read_text())
        production = yaml.safe_load((CHART / "examples/production.yaml").read_text())
        for values in ({}, production):
            with self.subTest(production=values.get("production", False)):
                values.setdefault("networkPolicy", {})["egress"] = []
                baseline = self.render(values)
                values["networkPolicy"].update(overlay["networkPolicy"])
                rendered = self.render(values)
                router_key = ("NetworkPolicy", NAME + "-router")
                original = baseline[router_key]["spec"]
                router = rendered[router_key]["spec"]
                self.assertEqual(router["egress"], original["egress"] + [{}])
                self.assertEqual(router["ingress"], original["ingress"])
                self.assertEqual(router["podSelector"], {
                    "matchLabels": rendered["Deployment", NAME]["spec"]["selector"]["matchLabels"],
                })
                for key, resource in baseline.items():
                    if key[0] == "NetworkPolicy" and key != router_key:
                        self.assertEqual(rendered[key], resource)

    def test_native_egress_rules_can_omit_destination_or_port_restrictions(self):
        for rule in ({"ports": [{"protocol": "TCP", "port": 443}]},
                     {"to": [{"ipBlock": {"cidr": "10.0.0.0/8"}}]},
                     {"to": [], "ports": []}):
            with self.subTest(rule=rule):
                resources = self.render({"networkPolicy": {"egress": [rule]}})
                self.assertEqual(resources["NetworkPolicy", NAME + "-router"]["spec"]["egress"][-1], rule)

    def test_invalid_values_fail_before_install(self):
        invalid = [{"config": {"admin_api": {"enabled": "yes"}}},
                   {"config": {"admin_api": {"max_lifetime_days": 366}}},
                   {"config": {"admin_api": {"max_concurrent": -1}}},
                   {"config": {"admin_api": {"trust_all_proxies": True}}},
                   {"replicaCount": 2}, {"postgresql": {"enabled": False}},
                   {"env": {"PORTAL_BASE_URL": "https://wrong.invalid"}},
                   {"env": {"OLR_TRACE_USER_ID": True}}, {"env": {"OIDC_ISSUER": "https://wrong.invalid"}},
                   {"env": {"OLR_DATABASE_URL": "postgres://wrong.invalid/olr"}},
                   {"secrets": {"bootstrapAdminPassword": "short"}},
                   {"secrets": {"apiKeyEncryptionKey": "invalid"}},
                   {"secrets": {"env": {"POSTGRES_PASSWORD": "collision"}}},
                   {"portal": {"cookieSecure": True}}, {"ingress": {"enabled": True}},
                   {"networkPolicy": {"egress": ["all"]}},
                   {"networkPolicy": {"egress": [{"to": "all"}]}},
                   {"networkPolicy": {"egress": [{"ports": "all"}]}}]
        for values in invalid:
            with self.subTest(values=values), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "values.yaml"
                path.write_text(yaml.safe_dump(values))
                result = self.invoke("template", "trial", CHART, "-f", path)
                self.assertNotEqual(result.returncode, 0)

    def test_lint_examples_and_package(self):
        for values in [None, "demo.values.yaml", "production.yaml", "traefik.values.yaml",
                       "unrestricted-egress.values.yaml"]:
            args = ["lint", CHART, "--strict"]
            if values:
                args += ["-f", CHART / "examples" / values]
                if values == "production.yaml":
                    args += ["--set", "image.digest=sha256:" + "a" * 64]
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
