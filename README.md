# open-llm-router

Small open-source LLM router for locally hosted models: OpenAI- and Anthropic-compatible API,
Keycloak SSO (any realm, incl. brokered federated IdPs), per-user API keys, group-based model access,
per-user token quotas and usage view, load-aware balancing across model replicas.

Status: **planning (plan v0.2)** — see [docs/PLAN.md](docs/PLAN.md).
Development runs fully local with Docker Compose (router, Postgres, local Keycloak, mock LLM);
Kubernetes manifests follow after the pilot.

Authors: A. Khalatyan (AIP), with Hermes agent.

## Citation

See [CITATION.cff](CITATION.cff) (GitHub: "Cite this repository").

## License

MIT — see [LICENSE](LICENSE).
