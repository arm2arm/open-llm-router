#!/usr/bin/env python3
"""Run isolated, assertion-based security mutations (requires PostgreSQL)."""

import argparse
from collections import defaultdict
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

from mutation_cases import CASES


def run_tests(root, package, tests):
    pattern = "^(" + "|".join(re.escape(t) for t in sorted(tests)) + ")$"
    result = subprocess.run(["go", "test", "-json", "-count=1", package, "-run", pattern],
                            cwd=root, capture_output=True, text=True, timeout=180)
    events = []
    for line in result.stdout.splitlines():
        try:
            events.append(json.loads(line))
        except ValueError:
            pass
    ran = {e["Test"].split("/")[0] for e in events if e.get("Action") == "run" and e.get("Test")}
    failed = [e["Test"] for e in events if e.get("Action") == "fail" and e.get("Test")]
    return result, ran, failed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", action="append", default=[], help="exact mutation name (repeatable)")
    parser.add_argument("--report", type=Path, help="write machine-readable outcomes")
    args = parser.parse_args()
    selected = [c for c in CASES if not args.case or c["name"] in args.case]
    if not selected or set(args.case) - {c["name"] for c in selected}:
        parser.error("unknown mutation name")
    if not os.environ.get("OLR_TEST_DATABASE_URL"):
        parser.error("OLR_TEST_DATABASE_URL is required; skipped database tests cannot kill a mutation")
    source = Path(__file__).resolve().parents[2]
    files = subprocess.check_output(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"], cwd=source).decode().split("\0")
    outcomes = []
    with tempfile.TemporaryDirectory(prefix="olr-security-mutations-") as directory:
        root = Path(directory)
        for name in filter(None, files):
            path = source / name
            if path.is_file():
                target = root / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(path, target)
        (root / "web/dist").mkdir(parents=True, exist_ok=True)
        (root / "web/dist/index.html").write_text("<!doctype html><title>Mutation fixture</title>")
        groups = defaultdict(set)
        for c in selected:
            groups[c["package"]].add(c["test"])
        for package, tests in groups.items():
            result, ran, _ = run_tests(root, package, tests)
            if result.returncode or ran != tests:
                raise SystemExit(f"Baseline failed or did not execute every test: {package}\n{result.stdout[-5000:]}\n{result.stderr[-2000:]}")
            print(f"baseline passed: {package} ({len(tests)} tests)", flush=True)
        for c in selected:
            originals = {}
            try:
                for edit in c["edits"]:
                    name, before, after, *counts = edit
                    path = root / name
                    originals.setdefault(path, path.read_text())
                    text = path.read_text()
                    count = counts[0] if counts else 1
                    if text.count(before) != count:
                        raise ValueError(f"{c['name']}: guard anchor changed in {name}; update mutation and verify its test")
                    path.write_text(text.replace(before, after))
                result, _, failed = run_tests(root, c["package"], {c["test"]})
                # Build errors, missing tests and test-process crashes are not
                # evidence that a security assertion detected a regression.
                killed = bool(result.returncode and failed)
                outcomes.append(dict(name=c["name"], killed=killed, failed_tests=failed))
                print(f"{'killed' if killed else 'SURVIVED/INVALID'}: {c['name']}", flush=True)
                if not killed:
                    print(result.stdout[-3000:] + result.stderr[-2000:], flush=True)
            finally:
                for path, text in originals.items():
                    path.write_text(text)
            restored, ran, _ = run_tests(root, c["package"], {c["test"]})
            if restored.returncode or ran != {c["test"]}:
                raise SystemExit(f"Restored baseline failed after {c['name']}; cannot attribute the failure to the mutation\n{restored.stdout[-3000:]}\n{restored.stderr[-1000:]}")
    if args.report:
        args.report.write_text(json.dumps(outcomes, indent=2) + "\n")
    killed = sum(c["killed"] for c in outcomes)
    print(f"{killed}/{len(outcomes)} security mutations killed by test assertions")
    return 0 if killed == len(outcomes) else 1


if __name__ == "__main__":
    raise SystemExit(main())
