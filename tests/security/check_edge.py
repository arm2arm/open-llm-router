#!/usr/bin/env python3
"""Exercise the runbook's actual NGINX source limits with spoofed forwarding headers."""

from pathlib import Path
import re
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[2]


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def main():
    runbook = (ROOT / "docs/RUNBOOK.md").read_text()
    example = re.search(r"```nginx\n(.*?)\n```", runbook, re.S).group(1)
    example = example.replace("listen 443 ssl;", "listen 8080;")
    example = example.replace("http://127.0.0.1:8080", "http://127.0.0.1:8081")
    upstream = "server { listen 8081; location / { return 401; } }"
    config = "events {}\nhttp { access_log off; error_log stderr crit;\n" + example + upstream + "\n}"
    name = "olr-edge-security-" + uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "nginx.conf"
        path.write_text(config)
        try:
            docker("run", "--detach", "--rm", "--name", name,
                   "--publish", "127.0.0.1::8080",
                   "--volume", str(path) + ":/etc/nginx/nginx.conf:ro", "nginx:stable-alpine")
            address = docker("port", name, "8080/tcp")
            url = "http://" + address + "/v1/models"

            def request(i):
                req = urllib.request.Request(url, headers={
                    "Authorization": "Bearer olr-" + str(i).zfill(43),
                    "X-Forwarded-For": "192.0.2." + str(i % 250 + 1),
                    "Forwarded": "for=198.51.100." + str(i % 250 + 1),
                    "X-Real-IP": "203.0.113." + str(i % 250 + 1),
                })
                try:
                    with urllib.request.urlopen(req, timeout=2) as resp:
                        return resp.status
                except urllib.error.HTTPError as error:
                    error.close()
                    return error.code

            deadline = time.monotonic() + 10
            while True:
                try:
                    assert request(0) == 401, "edge did not reach authentication upstream"
                    break
                except urllib.error.URLError:
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(0.1)
            statuses = [request(i) for i in range(1, 81)]
            assert set(statuses) == {401, 429}, statuses
            print("PASS: source throttling rejects rotating credentials and spoofed forwarding headers")
        finally:
            subprocess.run(["docker", "rm", "--force", name], check=False, capture_output=True)


if __name__ == "__main__":
    main()
