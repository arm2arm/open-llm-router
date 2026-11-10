# open-llm-router

Small open-source LLM router for locally hosted models: OpenAI- and Anthropic-compatible API,
Keycloak SSO (any realm, incl. brokered federated IdPs), per-user API keys, group-based model access,
per-user token quotas and usage view, load-aware balancing across model replicas.

Status: **planning (plan v0.3)** — requirements in [docs/PLAN.md](docs/PLAN.md), delivery plan in
[docs/WEEK1.md](docs/WEEK1.md), work items in the
[issues](https://github.com/arm2arm/open-llm-router/issues/1).
Stack: Go backend (one static binary), React portal, PostgreSQL, Keycloak.
Development runs fully local with Docker Compose (router, Postgres, local Keycloak, mock LLM);
Kubernetes manifests follow after the pilot.

Authors: A. Khalatyan (AIP), with Hermes agent.

## Citation

See [CITATION.cff](CITATION.cff) (GitHub: "Cite this repository").

## License

MIT — see [LICENSE](LICENSE).
