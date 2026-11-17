#!/usr/bin/env python3
"""Inventory pinned runtime images/tools and maintain one weekly review issue.

Dependabot handles root Dockerfile, Go, npm and Action pins. This supplements it
for Compose/Helm images and go-run/install tools, without auto-merging upgrades.
"""

import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess

TITLE = "[Maintenance] Review pinned image and tool updates"
MARKER = "<!-- olr-pinned-updates:"
IMAGES = {
    "postgres": "postgres/postgres",
    "quay.io/keycloak/keycloak": "keycloak/keycloak",
    "prom/prometheus": "prometheus/prometheus",
    "grafana/grafana": "grafana/grafana",
    "jaegertracing/jaeger": "jaegertracing/jaeger",
    "vllm/vllm-openai-cpu": "vllm-project/vllm",
    "node": "dependabot",
    "golang": "dependabot",
    "debian": "dependabot",
    "gcr.io/distroless/static-debian13": "dependabot",
}
TOOLS = {
    "github.com/sigstore/gitsign": "sigstore/gitsign",
    "github.com/zricethezav/gitleaks/v8": "gitleaks/gitleaks",
    "github.com/google/go-licenses/v2": "google/go-licenses",
    "honnef.co/go/tools/cmd/staticcheck": "dominikh/go-tools",
    "golang.org/x/vuln/cmd/govulncheck": "golang/vuln",
}


def api(endpoint, method="GET", payload=None):
    command = ["gh", "api", endpoint, "--method", method]
    if payload is not None:
        command += ["--input", "-"]
    result = subprocess.run(command, input=None if payload is None else json.dumps(payload),
                            capture_output=True, text=True, check=True, timeout=60)
    return json.loads(result.stdout) if result.stdout else None


def inventory(root):
    files = [root / "Dockerfile", root / "Makefile"]
    for directory in ["deploy", "charts", ".github/workflows"]:
        files += sorted(p for p in (root / directory).rglob("*") if p.suffix in {".yml", ".yaml"})
    images, tools = {}, {}
    for path in files:
        text = path.read_text()
        source = str(path.relative_to(root))
        for ref in re.findall(r"([\w./:-]+)@sha256:[a-f0-9]{64}\b", text):
            name = ref.rsplit(":", 1)[0] if ":" in ref.rsplit("/", 1)[-1] else ref
            if name not in IMAGES:
                raise ValueError(f"Untracked pinned image {name}: add its update source or explicit manual procedure")
            images.setdefault(ref, set()).add(source)
        for module, version in re.findall(r"(?:go (?:run|install) )([\w./-]+)@(v[\w.+-]+)", text):
            if module not in TOOLS:
                raise ValueError(f"Untracked pinned tool {module}: add its update source")
            tools.setdefault((module, version), set()).add(source)
    return images, tools


def latest(repository, fetch=api):
    # PostgreSQL publishes signed REL_* tags rather than GitHub Releases. The
    # vuln Go module uses module tags, also without GitHub Release records.
    if repository in {"postgres/postgres", "golang/vuln"}:
        tags = fetch(f"repos/{repository}/tags?per_page=100")
        pattern = r"REL_(\d+)_(\d+)$" if repository == "postgres/postgres" else r"v(\d+)\.(\d+)\.(\d+)$"
        stable = [(tuple(map(int, m.groups())), item["name"]) for item in tags if (m := re.fullmatch(pattern, item["name"]))]
        if not stable:
            raise ValueError(f"No stable tags found for {repository}")
        if repository == "postgres/postgres":
            # Include backports on older majors, even if the newest major did not change.
            majors = {}
            for version, name in stable:
                majors[version[0]] = max(majors.get(version[0], ((), "")), (version, name))
            return ", ".join(value[1] for _, value in sorted(majors.items(), reverse=True))
        return max(stable)[1]
    release = fetch(f"repos/{repository}/releases/latest")
    if release.get("prerelease") or release.get("draft") or not release.get("tag_name"):
        raise ValueError(f"No stable release found for {repository}")
    return release["tag_name"]


def review(root, fetch=api):
    images, tools = inventory(root)
    upstreams = sorted((set(IMAGES.values()) | set(TOOLS.values())) - {"dependabot"})
    releases = {name: latest(name, fetch) for name in upstreams}
    digest = hashlib.sha256(json.dumps(releases, sort_keys=True).encode()).hexdigest()
    lines = [f"{MARKER}{digest} -->", "Review new upstream releases against the repository's pinned images and tools. This issue is updated in place while open; reviewed snapshots are not reopened.",
             "", "| Upstream | Latest stable release/tag |", "| --- | --- |"]
    for name, version in releases.items():
        lines.append(f"| [{name}](https://github.com/{name}) | `{version}` |")
    lines += ["", "Pinned image tags (the source files also contain their required digests):"]
    for ref, files in sorted(images.items()):
        lines.append(f"- `{ref}` — {', '.join(sorted(files))}")
    lines += ["", "Pinned build and CI tools:"]
    for (module, version), files in sorted(tools.items()):
        lines.append(f"- `{module}@{version}` — {', '.join(sorted(files))}")
    lines += ["", "Follow [the update procedure](https://github.com/arm2arm/open-llm-router/blob/main/docs/RELEASE_SECURITY.md#pinned-image-and-tool-updates): verify upstream provenance, update version and digest together, run the matching image/client/render checks, and record the result. A new major version needs compatibility review; it is not an instruction to auto-upgrade.",
              "", "Root Dockerfile bases and Action SHAs also receive Dependabot PRs. The digest-only optional vLLM CPU fixture needs a deliberately selected upstream build and its client tests. Site-supplied deployment digests remain the operator's responsibility."]
    return digest, "\n".join(lines) + "\n"


def publish(repository, digest, body, fetch=api):
    if not re.fullmatch(r"[\w.-]+/[\w.-]+", repository):
        raise ValueError("invalid repository")
    # Walk every page: a reviewed snapshot remains reviewed after many releases.
    issues, page = [], 1
    while True:
        batch = fetch(f"repos/{repository}/issues?state=all&per_page=100&page={page}")
        issues += [i for i in batch if i.get("title") == TITLE and not i.get("pull_request") and MARKER in (i.get("body") or "") and i.get("user", {}).get("type") == "Bot"]
        if len(batch) < 100:
            break
        page += 1
    marker = f"{MARKER}{digest} -->"
    if any(marker in (i.get("body") or "") for i in issues):
        return "already reviewed or tracked"
    opened = [i for i in issues if i["state"] == "open"]
    if opened:
        fetch(f"repos/{repository}/issues/{opened[0]['number']}", "PATCH", {"body": body})
        return "updated existing review"
    fetch(f"repos/{repository}/issues", "POST", {"title": TITLE, "body": body})
    return "opened review"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--publish", metavar="OWNER/REPO", help="otherwise print a read-only report")
    args = parser.parse_args()
    digest, body = review(Path(__file__).resolve().parents[2])
    print(publish(args.publish, digest, body) if args.publish else body)


if __name__ == "__main__":
    main()
