#!/usr/bin/env python3
"""Helm post-renderer: verify every image before emitting any deployment YAML."""

import argparse
import json
import os
import re
import subprocess
import sys

import yaml


def images_in(value):
    if isinstance(value, dict):
        for key, child in value.items():
            if key in ("containers", "initContainers", "ephemeralContainers"):
                for container in child:
                    yield container["image"]
            else:
                yield from images_in(child)
    elif isinstance(value, list):
        for child in value:
            yield from images_in(child)


def verify(manifest, policy):
    images = sorted(set(images_in(list(yaml.safe_load_all(manifest)))))
    if not images:
        raise ValueError("no container images found")
    commands = []
    for image in images:
        match = re.fullmatch(r"([^@\s]+)@sha256:[a-f0-9]{64}", image)
        if not match:
            raise ValueError(f"mutable or invalid image: {image}")
        repository = match[1]
        # Ignore an optional human-readable tag; trust is scoped to the repository.
        if ":" in repository.rsplit("/", 1)[-1]:
            repository = repository.rsplit(":", 1)[0]
        trust = policy["images"][repository]
        identity = trust["certificate_identity"]
        issuer = trust["certificate_oidc_issuer"]
        if not identity or issuer != "https://token.actions.githubusercontent.com":
            raise ValueError("expected an exact GitHub Actions identity and issuer")
        commands.append(["cosign", "verify", "--certificate-identity", identity,
                         "--certificate-oidc-issuer", issuer, image])
        for predicate in ("https://slsa.dev/provenance/v1", "https://spdx.dev/Document"):
            commands.append([
                "gh", "attestation", "verify", "oci://" + image,
                "--repo", trust["repository"],
                "--signer-workflow", trust["signer_workflow"],
                "--cert-identity", identity, "--cert-oidc-issuer", issuer,
                "--source-ref", "refs/heads/main", "--deny-self-hosted-runners",
                "--predicate-type", predicate,
            ])
    # Validate the whole manifest's policy before starting network verification.
    for command in commands:
        subprocess.run(command, check=True, stdout=subprocess.DEVNULL, timeout=120)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--policy", default=os.environ.get("OLR_IMAGE_POLICY"), required=not os.environ.get("OLR_IMAGE_POLICY"))
    args = parser.parse_args()
    manifest = sys.stdin.read()
    try:
        with open(args.policy) as source:
            policy = json.load(source)
        verify(manifest, policy)
    except (KeyError, ValueError, TypeError, OSError, yaml.YAMLError,
            subprocess.SubprocessError) as error:
        print(f"image verification failed: {error}", file=sys.stderr)
        return 1
    sys.stdout.write(manifest)
    return 0


if __name__ == "__main__":
    sys.exit(main())
