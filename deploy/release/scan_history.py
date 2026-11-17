"""Scan every commit introduced by a pull request or push, including deleted files."""
import json
import os
from pathlib import Path
import re
import subprocess


def revision_range(event, head):
    if not re.fullmatch(r"[a-fA-F0-9]{40}", head):
        raise ValueError("expected a full Git commit SHA")
    base = event.get("pull_request", {}).get("base", {}).get("sha") or event.get("before")
    if not base or base == "0" * 40:
        return "--all"
    if not re.fullmatch(r"[a-fA-F0-9]{40}", base):
        raise ValueError("expected a full base commit SHA")
    return base + ".." + head


def scan(directory, revisions):
    return subprocess.run(["gitleaks", "git", "--redact", "--no-banner", "--log-opts=" + revisions],
                          cwd=directory, text=True, capture_output=True)


if __name__ == "__main__":
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    revisions = revision_range(event, os.environ["GITHUB_SHA"])
    result = scan(Path.cwd(), revisions)
    print(result.stdout, end="")
    print(result.stderr, end="")
    raise SystemExit(result.returncode)
