"""Check the disposable CI Compose stack's real scrape with a custom token.

Run after the SDK/load fixtures. This restarts the test router.
"""
import json
import os
from pathlib import Path
import subprocess
import time
from urllib.parse import urlencode

root = Path(__file__).resolve().parents[2]
env = {**os.environ, "OLR_METRICS_TOKEN": "synthetic-custom-scrape-test-token"}
compose = ["docker", "compose"]
if (root / ".env").exists():
    compose += ["--env-file", ".env"]
compose += ["-f", "deploy/compose/docker-compose.yml", "-f", "deploy/compose/observability.yml"]
subprocess.run(compose + ["up", "--detach", "--wait", "--wait-timeout", "90", "router", "prometheus"],
               cwd=root, env=env, check=True)
subprocess.run(compose + ["exec", "-T", "prometheus", "promtool", "check", "rules", "/etc/prometheus/router-alerts.yml"], cwd=root, env=env, check=True)
subprocess.run(compose + ["exec", "-T", "--workdir", "/etc/prometheus", "prometheus", "promtool", "test", "rules", "router-alerts.test.yml"], cwd=root, env=env, check=True)

def query(expression):
    url = "http://127.0.0.1:9090/api/v1/query?" + urlencode({"query": expression})
    result = subprocess.run(compose + ["exec", "-T", "prometheus", "wget", "-qO-", url],
                            cwd=root, env=env, text=True, capture_output=True, check=False)
    if result.returncode:
        return []
    data = json.loads(result.stdout)
    assert data["status"] == "success"
    return data["data"]["result"]

for attempt in range(30):
    up = query('up{job="olr"}')
    if up and up[0]["value"][1] == "1" and query('olr_usage_dropped_total{job="olr"}'):
        print("Custom metrics token: router scrape UP; dashboard usage series available")
        break
    time.sleep(2)
else:
    raise SystemExit("Custom-token scrape failed or dashboard usage metric missing")
