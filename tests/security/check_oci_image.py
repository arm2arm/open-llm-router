"""Inspect a cross-built OCI archive without executing a target-architecture process."""
import argparse
import json
import tarfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("archive")
parser.add_argument("--architecture", required=True, choices=("amd64", "arm64"))
parser.add_argument("--version", required=True)
parser.add_argument("--commit", required=True)
args = parser.parse_args()

with tarfile.open(args.archive) as archive:
    def document(name):
        return json.load(archive.extractfile(name))

    def blob(descriptor):
        return "blobs/" + descriptor["digest"].replace(":", "/")

    def manifests(index):
        for descriptor in index["manifests"]:
            value = document(blob(descriptor))
            if "manifests" in value:
                yield from manifests(value)
            elif "layers" in value:
                yield value

    matches = []
    for manifest in manifests(document("index.json")):
        config = document(blob(manifest["config"]))
        if config.get("os") == "linux" and config.get("architecture") == args.architecture:
            matches.append((manifest, config))
    assert len(matches) == 1, "expected one image for the target architecture"
    manifest, config = matches[0]
    labels = config["config"]["Labels"]
    assert labels["org.opencontainers.image.version"] == args.version
    assert labels["org.opencontainers.image.revision"] == args.commit
    assert labels["org.opencontainers.image.source"] == "https://github.com/arm2arm/open-llm-router"
    assert labels["org.opencontainers.image.licenses"] == "MIT"
    notices = {}
    machines = {}
    for layer in manifest["layers"]:
        with tarfile.open(fileobj=archive.extractfile(blob(layer)), mode="r|*") as files:
            for member in files:
                name = member.name.removeprefix("./").lstrip("/")
                if member.isfile() and name in ("licenses/LICENSE", "licenses/GO-LICENSE", "licenses/THIRD_PARTY_NOTICES.txt"):
                    notices[name] = files.extractfile(member).read().decode()
                if member.isfile() and name in ("usr/local/bin/olr", "usr/bin/bash", "usr/bin/curl"):
                    header = files.extractfile(member).read(20)
                    assert header[:4] == b"\x7fELF"
                    machines[name] = int.from_bytes(header[18:20], "little" if header[5] == 1 else "big")
    expected_machine = {"amd64": 62, "arm64": 183}[args.architecture]
    for binary in ("usr/local/bin/olr", "usr/bin/bash", "usr/bin/curl"):
        assert machines.get(binary) == expected_machine, f"{binary} missing or architecture differs from manifest"
    assert "MIT License" in notices["licenses/LICENSE"]
    assert "Redistribution" in notices["licenses/GO-LICENSE"]
    text = notices["licenses/THIRD_PARTY_NOTICES.txt"]
    assert "react@" in text and "prometheus" in text and len(text) > 10000
print("OCI router/Bash/curl architectures, release labels and Go/portal license texts verified")
