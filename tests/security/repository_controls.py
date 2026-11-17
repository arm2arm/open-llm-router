"""Apply (only with --apply) and verify actual GitHub repository controls."""

import argparse
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[2]


def api(path, *args):
    output = subprocess.check_output(["gh", "api", path, *args], text=True)
    return json.loads(output) if output.strip() else None


def verify_branch(actual, expected):
    for key in ("enforce_admins", "required_conversation_resolution", "allow_force_pushes", "allow_deletions"):
        assert actual[key]["enabled"] == expected[key], key
    reviews = actual["required_pull_request_reviews"]
    for key, value in expected["required_pull_request_reviews"].items():
        assert reviews[key] == value, key
    assert not any(reviews.get("bypass_pull_request_allowances", {}).values()), "PR bypass allowances"
    assert actual["required_status_checks"]["strict"] is True
    checks = lambda value: {(item["context"], item["app_id"]) for item in value["checks"]}
    assert checks(actual["required_status_checks"]) == checks(expected["required_status_checks"])


def verify_tags(actual, expected):
    for key in ("target", "enforcement", "conditions"):
        assert actual[key] == expected[key], "release tag " + key
    assert not actual.get("bypass_actors"), "release tag bypass actors"
    assert {"update", "deletion"} <= {rule["type"] for rule in actual["rules"]}, "mutable release tags"


def verify_approval(environment, policy):
    if not policy["independent_approval"]:
        return
    assert environment, "release approval environment missing"
    reviews = next((rule for rule in environment["protection_rules"] if rule["type"] == "required_reviewers"), {})
    assert reviews.get("reviewers") and reviews.get("prevent_self_review"), "independent release approval required"


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
    verify_branch(actual, expected)
    tag_file = ROOT / ".github/release-tags.json"
    tags = json.loads(tag_file.read_text())
    rulesets_path = f"repos/{args.repository}/rulesets"
    matching = [item for item in api(rulesets_path) if item["name"] == tags["name"]]
    assert len(matching) <= 1, "duplicate release rulesets"
    tag_path = rulesets_path + ("/" + str(matching[0]["id"]) if matching else "")
    if args.apply:
        applied = api(tag_path, "--method", "PUT" if matching else "POST", "--input", str(tag_file))
        tag_path = rulesets_path + "/" + str(applied["id"])
    else:
        assert matching, "release tag ruleset missing"
    verify_tags(api(tag_path), tags)
    approval = json.loads((ROOT / ".github/release-approval.json").read_text())
    environments = api(f"repos/{args.repository}/environments?per_page=100")["environments"]
    environment = next((item for item in environments if item["name"] == approval["environment"]), None)
    if environment:
        environment = api(f"repos/{args.repository}/environments/{approval['environment']}")
    verify_approval(environment, approval)
    settings = api(f"repos/{args.repository}")["security_and_analysis"]
    for key in ("secret_scanning", "secret_scanning_push_protection", "dependabot_security_updates"):
        assert settings[key]["status"] == "enabled", key
    assert api(f"repos/{args.repository}/actions/permissions")["sha_pinning_required"] is True
    private_reporting = f"repos/{args.repository}/private-vulnerability-reporting"
    if args.apply and not api(private_reporting)["enabled"]:
        api(private_reporting, "--method", "PUT")
    assert api(private_reporting)["enabled"], "private vulnerability reporting disabled"
    print("Verified main/release-tag protections, configured approval mode, private reporting, secret scanning, Dependabot and Action SHA policy")


if __name__ == "__main__":
    main()
