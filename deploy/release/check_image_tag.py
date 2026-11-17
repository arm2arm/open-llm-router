"""Refuse to replace an existing GHCR version, including after partial releases."""
import base64
import json
import os
import re
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen


def require_unused(image, version, actor, credential, open_url=urlopen):
    if not re.fullmatch(r"ghcr\.io/[a-z0-9._/-]+", image) or not re.fullmatch(r"v\d+\.\d+\.\d+", version):
        raise ValueError("expected a GHCR repository and semantic release version")
    repository = image.removeprefix("ghcr.io/")
    authorization = base64.b64encode((actor + ":" + credential).encode()).decode()
    token_url = "https://ghcr.io/token?" + urlencode({"service": "ghcr.io", "scope": "repository:" + repository + ":pull"})
    with open_url(Request(token_url, headers={"Authorization": "Basic " + authorization}), timeout=30) as response:
        token = json.load(response)["token"]
    request = Request("https://ghcr.io/v2/" + repository + "/manifests/" + version, method="HEAD",
                      headers={"Authorization": "Bearer " + token,
                               "Accept": "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"})
    try:
        with open_url(request, timeout=30):
            raise ValueError("release image tag already exists; choose a new version")
    except HTTPError as error:
        if error.code != 404:
            raise ValueError("cannot establish that the release image tag is unused") from None


if __name__ == "__main__":
    try:
        require_unused(os.environ["IMAGE"], os.environ["VERSION"], os.environ["GITHUB_ACTOR"], os.environ["GH_TOKEN"])
    except (ValueError, OSError, KeyError):
        raise SystemExit("Release image version exists or could not be verified unused; publication stopped") from None
    print("Verified unused registry version")
