"""Apply (only with --apply) and verify actual GitHub repository controls."""

import argparse
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[2]


def api(path, *args):
    return json.loads(subprocess.check_output(["gh", "api", path, *args], text=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--repository", default="arm2arm/open-llm-router")
    args = parser.parse_args()
    path = f"repos/{args.repository}/branches/main/protection"
    policy_file = ROOT / ".github/main-protection.json"
    expected = json.loads(policy_file.read_text())
    if args.apply:
        api(path, "--method", "PUT", "--input", str(policy_file))
    actual = api(path)
    for key in ("enforce_admins", "required_conversation_resolution", "allow_force_pushes", "allow_deletions"):
        assert actual[key]["enabled"] == expected[key], key
    for key, value in expected["required_pull_request_reviews"].items():
        assert actual["required_pull_request_reviews"][key] == value, key
    assert actual["required_status_checks"]["strict"] is True
    checks = lambda value: {(item["context"], item["app_id"]) for item in value["checks"]}
    assert checks(actual["required_status_checks"]) == checks(expected["required_status_checks"])
    settings = api(f"repos/{args.repository}")["security_and_analysis"]
    for key in ("secret_scanning", "secret_scanning_push_protection", "dependabot_security_updates"):
        assert settings[key]["status"] == "enabled", key
    assert api(f"repos/{args.repository}/actions/permissions")["sha_pinning_required"] is True
    print("Verified main protections, secret scanning/push protection, Dependabot and Action SHA policy")


if __name__ == "__main__":
    main()
