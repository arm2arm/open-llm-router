"""Exercise fail-closed deployment verification with synthetic verifier binaries."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
VERIFIER = ROOT / "deploy/release/verify_images.py"
IMAGE = "ghcr.io/arm2arm/open-llm-router@sha256:" + "a" * 64


class DeploymentVerification(unittest.TestCase):
    def invoke(self, manifest, fail="", policy=None):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            for executable in ("cosign", "gh"):
                binary = path / executable
                binary.write_text(
                    "#!/usr/bin/env python3\nimport json, os, sys\n"
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
            result = subprocess.run([sys.executable, str(VERIFIER), "--policy", str(policy_path)],
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
            self.assertIn("--signer-workflow", command)
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


class WorkflowPolicy(unittest.TestCase):
    def test_actions_are_commit_pinned(self):
        for path in (ROOT / ".github/workflows").glob("*.yml"):
            document = yaml.safe_load(path.read_text())
            for job in document["jobs"].values():
                for step in job.get("steps", []):
                    if "uses" in step:
                        self.assertRegex(step["uses"], r"^[\w./-]+@[a-f0-9]{40}$", str(path))


if __name__ == "__main__":
    unittest.main()
