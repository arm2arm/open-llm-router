"""Exercise fail-closed deployment verification with synthetic verifier binaries."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
import copy
import io
from urllib.error import HTTPError

import yaml
import repository_controls as controls

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "deploy/release"))
import check_image_tag
VERIFIER = ROOT / "deploy/release/verify_images.py"
IMAGE = "ghcr.io/arm2arm/open-llm-router@sha256:" + "a" * 64


class DeploymentVerification(unittest.TestCase):
    def invoke(self, manifest, fail="", policy=None, helm=False):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            for executable in ("cosign", "gh"):
                binary = path / executable
                binary.write_text(
                    "#!/usr/bin/env python3\nimport json, os, sys\n"
                    "identity_flags = {'--cert-identity', '--cert-identity-regex', '--signer-repo', '--signer-workflow'}\n"
                    "if sys.argv[0].endswith('/gh') and len(identity_flags.intersection(sys.argv)) > 1: sys.exit(2)\n"
                    "with open(os.environ['CALLS'], 'a') as output:\n"
                    "    output.write(json.dumps(sys.argv) + '\\n')\n"
                    "sys.exit(1 if os.environ.get('FAIL') in (sys.argv[0].rsplit('/', 1)[-1], 'all') else 0)\n"
                )
                binary.chmod(0o755)
            policy_path = path / "policy.json"
            policy_path.write_text(json.dumps(policy) if policy is not None else (ROOT / "deploy/release/image-policy.json").read_text())
            calls = path / "calls"
            env = dict(os.environ, PATH=directory + os.pathsep + os.environ["PATH"],
                       CALLS=str(calls), FAIL=fail)
            command = [sys.executable, str(VERIFIER), "--policy", str(policy_path)]
            if helm:
                plugins = path / "plugins"
                plugins.mkdir()
                (plugins / "olr-verify-images").symlink_to(VERIFIER.parent, target_is_directory=True)
                env.update(HELM_PLUGINS=str(plugins), OLR_IMAGE_POLICY=str(policy_path))
                env["PATH"] = str(Path(sys.executable).parent) + os.pathsep + env["PATH"]
                command = [os.environ.get("HELM", "helm"), "template", "trial", str(ROOT / "charts/open-llm-router"),
                           "-f", str(ROOT / "charts/open-llm-router/examples/production.yaml"),
                           "--set", "image.repository=ghcr.io/arm2arm/open-llm-router",
                           "--set", "image.digest=sha256:" + "a" * 64,
                           "--post-renderer", "olr-verify-images"]
            result = subprocess.run(command,
                                    input=manifest, text=True, capture_output=True, env=env)
            commands = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
            return result, commands

    def pod(self, image=IMAGE, **extra):
        return yaml.safe_dump({"kind": "Pod", "spec": {"containers": [{"name": "router", "image": image}], **extra}})

    def test_emits_exact_manifest_only_after_all_verifications(self):
        manifest = self.pod()
        result, commands = self.invoke(manifest)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, manifest)
        self.assertEqual(len(commands), 3)
        self.assertEqual(Path(commands[0][0]).name, "cosign")
        for command in commands[1:]:
            self.assertIn("--cert-identity", command)
            self.assertNotIn("--signer-workflow", command)
            self.assertIn("--deny-self-hosted-runners", command)
            self.assertIn("refs/heads/main", command)
        self.assertEqual({command[-1] for command in commands[1:]},
                         {"https://slsa.dev/provenance/v1", "https://spdx.dev/Document"})

    def test_rejects_mutable_unknown_and_untrusted_images_before_network(self):
        for image in ("router:latest", IMAGE.replace("open-llm-router", "unknown")):
            with self.subTest(image=image):
                result, commands = self.invoke(self.pod(image))
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertFalse(commands)
        result, commands = self.invoke(self.pod(), policy={"images": {}})
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(commands)

    def test_no_manifest_on_signature_or_attestation_failure(self):
        for fail in ("cosign", "gh"):
            with self.subTest(fail=fail):
                result, _ = self.invoke(self.pod(), fail=fail)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")

    def test_nested_jobs_init_and_ephemeral_images_cannot_bypass_policy(self):
        for key in ("initContainers", "ephemeralContainers"):
            with self.subTest(key=key):
                pod = yaml.safe_load(self.pod(**{key: [{"name": "helper", "image": "helper:mutable"}]}))
                manifest = yaml.safe_dump({"kind": "CronJob", "spec": {"jobTemplate": {"spec": {"template": pod}}}})
                result, commands = self.invoke(manifest)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertFalse(commands)

    def test_empty_manifest_fails(self):
        result, _ = self.invoke("---\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")

    @unittest.skipUnless(shutil.which(os.environ.get("HELM", "helm")), "requires Helm 4")
    def test_real_helm_plugin_gates_rendered_production_resources(self):
        result, commands = self.invoke("", helm=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("kind: Deployment", result.stdout)
        self.assertEqual(len(commands), 3)
        denied, _ = self.invoke("", fail="gh", helm=True)
        self.assertNotEqual(denied.returncode, 0)
        self.assertEqual(denied.stdout, "")


class WorkflowPolicy(unittest.TestCase):
    def test_registry_version_check_is_before_build_and_credentials_are_scoped(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/release.yml").read_text())
        release = workflow["jobs"]["release"]
        self.assertEqual(release["permissions"]["contents"], "read")
        self.assertNotIn("GH_TOKEN", release.get("env", {}))
        steps = release["steps"]
        check = next(i for i, step in enumerate(steps) if step.get("run") == "python3 deploy/release/check_image_tag.py")
        build = next(i for i, step in enumerate(steps) if step.get("id") == "build")
        self.assertLess(check, build)
        publish = workflow["jobs"]["publish"]
        self.assertEqual(set(publish["needs"]), {"release", "signing-tool"})
        self.assertNotIn("GH_TOKEN", publish.get("env", {}))
        for job in workflow["jobs"].values():
            if job.get("permissions", {}).get("contents") == "write":
                self.assertNotIn("go install", json.dumps(job))
                self.assertNotIn("curl", json.dumps(job))
        self.assertEqual(release["environment"], "release")

    def test_current_gh_accepts_exact_attestation_identity_flags(self):
        self.assertIsNotNone(shutil.which("gh"), "release verification requires GitHub CLI")
        trust = json.loads((ROOT / "deploy/release/image-policy.json").read_text())["images"]["ghcr.io/arm2arm/open-llm-router"]
        with tempfile.TemporaryDirectory() as directory:
            absent = str(Path(directory) / "absent-artifact")
            result = subprocess.run([
                "gh", "attestation", "verify", absent, "--repo", trust["repository"],
                "--cert-identity", trust["certificate_identity"],
                "--cert-oidc-issuer", trust["certificate_oidc_issuer"],
                "--source-ref", "refs/heads/main", "--deny-self-hosted-runners",
            ], text=True, capture_output=True, timeout=45)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("failed to open local artifact", result.stderr)
        self.assertNotIn("none of the others", result.stderr)

    def test_release_uses_same_exact_identity_selector(self):
        source = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn('--cert-identity "$IDENTITY"', source)
        self.assertNotIn("--signer-workflow", source)
        self.assertNotIn("--signer-repo", source)

    def test_actions_are_commit_pinned(self):
        for path in (ROOT / ".github/workflows").glob("*.yml"):
            document = yaml.safe_load(path.read_text())
            for job in document["jobs"].values():
                for step in job.get("steps", []):
                    if "uses" in step:
                        self.assertRegex(step["uses"], r"^[\w./-]+@[a-f0-9]{40}$", str(path))


class RepositoryControls(unittest.TestCase):
    def test_stubbed_branch_response_rejects_all_pr_bypass_types(self):
        expected = json.loads((ROOT / ".github/main-protection.json").read_text())
        actual = copy.deepcopy(expected)
        for key in ("enforce_admins", "required_conversation_resolution", "allow_force_pushes", "allow_deletions"):
            actual[key] = {"enabled": actual[key]}
        controls.verify_branch(actual, expected)
        for category in ("users", "teams", "apps"):
            changed = copy.deepcopy(actual)
            changed["required_pull_request_reviews"]["bypass_pull_request_allowances"] = {category: [{"id": 123}]}
            with self.subTest(category=category), self.assertRaisesRegex(AssertionError, "bypass"):
                controls.verify_branch(changed, expected)

    def test_tag_mutations_and_optional_independent_approval(self):
        expected = json.loads((ROOT / ".github/release-tags.json").read_text())
        controls.verify_tags(expected, expected)
        for change in ({"enforcement": "disabled"}, {"bypass_actors": [{"actor_id": 1}]},
                       {"rules": [{"type": "update"}]},
                       {"conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": ["refs/tags/v1*"]}}}):
            with self.subTest(change=change), self.assertRaises(AssertionError):
                controls.verify_tags({**expected, **change}, expected)
        controls.verify_approval(None, {"independent_approval": False})
        with self.assertRaises(AssertionError):
            controls.verify_approval(None, {"independent_approval": True})
        environment = {"protection_rules": [{"type": "required_reviewers", "prevent_self_review": True,
                                              "reviewers": [{"reviewer": {"id": 123}}]}]}
        controls.verify_approval(environment, {"independent_approval": True})
        environment["protection_rules"][0]["prevent_self_review"] = False
        with self.assertRaises(AssertionError):
            controls.verify_approval(environment, {"independent_approval": True})


class RegistryVersions(unittest.TestCase):
    def test_existing_and_unknown_versions_fail_closed(self):
        for status in (200, 404, 401, 403, 429, 500):
            calls = []
            def registry(request, timeout):
                calls.append(request)
                if len(calls) == 1:
                    return io.BytesIO(b'{"token":"synthetic-registry-token"}')
                if status == 200:
                    return io.BytesIO(b"")
                raise HTTPError(request.full_url, status, "synthetic", {}, None)
            with self.subTest(status=status):
                if status == 404:
                    check_image_tag.require_unused("ghcr.io/example/router", "v1.2.3", "test", "synthetic", registry)
                else:
                    with self.assertRaises(ValueError):
                        check_image_tag.require_unused("ghcr.io/example/router", "v1.2.3", "test", "synthetic", registry)
                self.assertEqual(calls[1].get_method(), "HEAD")
                self.assertEqual(calls[1].full_url, "https://ghcr.io/v2/example/router/manifests/v1.2.3")


if __name__ == "__main__":
    unittest.main()
