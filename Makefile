# Tools run at pinned versions without becoming dependencies of the module.
STATICCHECK := go run honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
COMPOSE     := docker compose
SDK_RUN     ?= uv run

.PHONY: build lint vuln test test-clients up smoke sdk sdk-limits sdk-real bench-real down

build:
	CGO_ENABLED=0 go build -trimpath -o bin/olr ./cmd/olr
	CGO_ENABLED=0 go build -trimpath -o bin/mock-llm ./cmd/mock-llm

lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	$(STATICCHECK) ./...

vuln:
	$(GOVULNCHECK) ./...

# Socket and database checks must run against the current services each time.
test:
	go test -race -count=1 ./...

test-clients:
	python3 -m unittest discover -s tests/clients -p 'test_*.py'

up:
	$(COMPOSE) up --build --detach --wait --wait-timeout 60

# Resolve actual published ports so .env overrides work here too.
smoke:
	@user_id="$$($(COMPOSE) exec -T router olr admin user add -username smoke-$$(date +%s)-$$$$ -groups staff)" || exit $$?; \
	trap '$(COMPOSE) exec -T router olr admin user disable -id "$$user_id" >/dev/null || true' EXIT; \
	key="$$($(COMPOSE) exec -T router olr admin key create -user "$$user_id" -label smoke)" || exit $$?; \
	for probe in "router 8080 /healthz" "router 8080 /readyz" "router 8080 /v1/models" "mock-llm-a 8000 /health" "mock-llm-b 8000 /health"; do \
		set -- $$probe; \
		address="$$($(COMPOSE) port $$1 $$2)" || exit $$?; \
		curl --fail --silent --show-error --retry 20 --retry-all-errors \
			-H "Authorization: Bearer $$key" \
			--retry-delay 1 --retry-max-time 30 --max-time 2 "http://$$address$$3" || exit $$?; \
	done

# The official SDK scripts run through the router and directly against each mock.
# Locally uv supplies their pinned dependencies; CI provides a Python venv.
sdk:
	@user_id="$$($(COMPOSE) exec -T router olr admin user add -username sdk-$$(date +%s)-$$$$ -groups staff)" || exit $$?; \
	trap '$(COMPOSE) exec -T router olr admin user disable -id "$$user_id" >/dev/null || true' EXIT; \
	key="$$($(COMPOSE) exec -T router olr admin key create -user "$$user_id" -label sdk)" || exit $$?; \
	stats_a="$$($(COMPOSE) port mock-llm-a 8000)" || exit $$?; \
	stats_b="$$($(COMPOSE) port mock-llm-b 8000)" || exit $$?; \
	for service in router mock-llm-a mock-llm-b; do \
		port=8000; if [ "$$service" = router ]; then port=8080; fi; \
		address="$$($(COMPOSE) port $$service $$port)" || exit $$?; \
		stats="http://$$address/_mock/stats"; \
		api_key=olr-not-checked; \
		if [ "$$service" = router ]; then stats="http://$$stats_a/_mock/stats,http://$$stats_b/_mock/stats"; api_key="$$key"; fi; \
		OLR_BASE_URL="http://$$address" OLR_API_KEY="$$api_key" OLR_EMBEDDING_MODEL=chat-model OLR_TEST_MOCK=1 \
			OLR_CHECK_EXTENDED=1 OLR_REQUIRE_CANCEL_EVIDENCE=1 OLR_CANCEL_STATS_URLS="$$stats" OLR_VLLM_METRICS_URL= \
			$(SDK_RUN) tests/clients/check_openai.py || exit $$?; \
		OLR_BASE_URL="http://$$address" OLR_API_KEY="$$api_key" OLR_CHECK_THINKING=1 OLR_TEST_MOCK=1 \
			OLR_CHECK_EXTENDED=1 OLR_REQUIRE_CANCEL_EVIDENCE=1 OLR_CANCEL_STATS_URLS="$$stats" OLR_VLLM_METRICS_URL= \
			$(SDK_RUN) tests/clients/check_anthropic.py || exit $$?; \
	done

# A separate user keeps this test independent of the successful SDK traffic.
sdk-limits:
	@user_id="$$($(COMPOSE) exec -T router olr admin user add -username limits-sdk-$$(date +%s)-$$$$ -groups students)" || exit $$?; \
	trap '$(COMPOSE) exec -T router olr admin user disable -id "$$user_id" >/dev/null || true' EXIT; \
	key="$$($(COMPOSE) exec -T router olr admin key create -user "$$user_id" -label limits-sdk)" || exit $$?; \
	address="$$($(COMPOSE) port router 8080)" || exit $$?; \
	OLR_BASE_URL="http://$$address" OLR_API_KEY="$$key" $(SDK_RUN) tests/clients/check_limits.py

# Existing host-side router configured with config.real.example.yaml; no Compose dependency.
# Set OLR_REQUIRE_CANCEL_EVIDENCE=1 for the vLLM go/no-go run.
sdk-real:
	@test -n "$$OLR_BASE_URL" && test -n "$$OLR_DIRECT_BASE_URL" && test -n "$$OLR_UPSTREAM_MODEL" || \
		{ echo 'Set OLR_BASE_URL, OLR_DIRECT_BASE_URL and OLR_UPSTREAM_MODEL (docs/CLIENTS.md)'; exit 1; }
	@status=0; \
	for script in check_openai.py check_anthropic.py; do \
		OLR_BASE_URL="$$OLR_DIRECT_BASE_URL" OLR_MODEL="$$OLR_UPSTREAM_MODEL" \
			OLR_API_KEY="$${OLR_DIRECT_API_KEY:-olr-not-checked}" OLR_CHECK_EXTENDED=1 \
			$(SDK_RUN) tests/clients/$$script || status=1; \
		OLR_CHECK_EXTENDED=1 $(SDK_RUN) tests/clients/$$script || status=1; \
	done; exit $$status

bench-real:
	$(SDK_RUN) tests/clients/compare_stream.py

down:
	$(COMPOSE) down
