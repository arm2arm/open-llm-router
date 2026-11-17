"""Prove credentials deleted later in the same PR remain detectable in history."""
import importlib.util
from pathlib import Path
import secrets
import shutil
import string
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("scan_history", ROOT / "deploy/release/scan_history.py")
history = importlib.util.module_from_spec(spec)
spec.loader.exec_module(history)


class SecretHistory(unittest.TestCase):
    def test_public_example_exception_does_not_exempt_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            shutil.copy2(ROOT / ".gitleaks.toml", root / ".gitleaks.toml")
            example = root / "deploy/compose/pilot.env.example"
            example.parent.mkdir(parents=True)
            def git(*args):
                return subprocess.check_output(["git", "-c", "core.hooksPath=/dev/null", *args], cwd=root, stderr=subprocess.DEVNULL)
            git("init", "--initial-branch=main")
            git("config", "user.name", "Synthetic history test")
            git("config", "user.email", "fixture@example.invalid")
            def scan():
                git("add", ".")
                git("-c", "commit.gpgsign=false", "commit", "-m", "Synthetic example fixture")
                return history.scan(root, "--all")
            example.write_text("OIDC_PORTAL_CLIENT_SECRET=\nOIDC_AUDIENCE=olr-api\n")
            self.assertEqual(scan().returncode, 0, "public example assignment was classified as a secret")
            # The same file/path is still scanned for actual credential shapes.
            value = "ghp_" + "".join(secrets.choice(string.ascii_letters + string.digits) for _ in range(36))
            example.write_text("OIDC_PORTAL_CLIENT_SECRET=" + value + "\nOIDC_AUDIENCE=olr-api\n")
            result = scan()
            self.assertEqual(result.returncode, 1, "example exception hid a credential")
            self.assertNotIn(value, result.stdout + result.stderr)

    def test_deleted_synthetic_token_is_detected_only_in_history(self):
        self.assertIsNotNone(shutil.which("gitleaks"), "install the pinned Gitleaks tool first")
        with tempfile.TemporaryDirectory() as directory:
            def git(*args):
                return subprocess.check_output(["git", "-c", "core.hooksPath=/dev/null", *args], cwd=directory, text=True, stderr=subprocess.DEVNULL).strip()
            git("init", "--initial-branch=main")
            git("config", "user.name", "Synthetic history test")
            git("config", "user.email", "fixture@example.invalid")
            git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "baseline")
            base = git("rev-parse", "HEAD")
            # Random, non-live credential-shaped data stays inside this disposable
            # repository and is never printed, uploaded or used for authentication.
            value = "gh" + "p_" + "".join(secrets.choice(string.ascii_letters + string.digits) for _ in range(36))
            path = Path(directory) / "fixture.env"
            path.write_text("GITHUB_TOKEN=" + value + "\n")
            git("add", "fixture.env")
            git("-c", "commit.gpgsign=false", "commit", "-m", "add synthetic fixture")
            path.unlink()
            git("add", "-u")
            git("-c", "commit.gpgsign=false", "commit", "-m", "remove synthetic fixture")
            final = subprocess.run(["gitleaks", "dir", "--redact", "--no-banner"], cwd=directory, text=True, capture_output=True)
            self.assertEqual(final.returncode, 0, "final tree unexpectedly contains fixture data")
            revisions = history.revision_range({"pull_request": {"base": {"sha": base}}}, git("rev-parse", "HEAD"))
            result = history.scan(directory, revisions)
            self.assertEqual(result.returncode, 1, "history scan missed a deleted credential-shaped fixture")
            self.assertNotIn(value, result.stdout + result.stderr)

    def test_ranges_reject_non_shas_and_cover_initial_pushes(self):
        head = "a" * 40
        self.assertEqual(history.revision_range({"before": "0" * 40}, head), "--all")
        self.assertEqual(history.revision_range({"before": "b" * 40}, head), "b" * 40 + ".." + head)
        with self.assertRaises(ValueError):
            history.revision_range({"before": "--all --unsafe"}, head)


if __name__ == "__main__":
    unittest.main()
