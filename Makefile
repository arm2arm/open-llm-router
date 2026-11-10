# Tools run at pinned versions without becoming dependencies of the module.
STATICCHECK := go run honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
COMPOSE     := docker compose
SDK_RUN     ?= uv run

.PHONY: build lint vuln test up smoke sdk down

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

up:
	$(COMPOSE) up --build --detach --wait --wait-timeout 60

# Resolve actual published ports so .env overrides work here too.
smoke:
	@for probe in "router 8080 /healthz" "router 8080 /readyz" "router 8080 /v1/models" "mock-llm-a 8000 /health" "mock-llm-b 8000 /health"; do \
		set -- $$probe; \
		address="$$($(COMPOSE) port $$1 $$2)" || exit $$?; \
		curl --fail --silent --show-error --retry 20 --retry-all-errors \
			--retry-delay 1 --retry-max-time 30 --max-time 2 "http://$$address$$3" || exit $$?; \
	done

# The official SDK scripts run through the router and directly against each mock.
# Locally uv supplies their pinned dependencies; CI provides a Python venv.
sdk:
	@for service in router mock-llm-a mock-llm-b; do \
		port=8000; if [ "$$service" = router ]; then port=8080; fi; \
		address="$$($(COMPOSE) port $$service $$port)" || exit $$?; \
		OLR_BASE_URL="http://$$address" OLR_EMBEDDING_MODEL=chat-model \
			$(SDK_RUN) tests/clients/check_openai.py || exit $$?; \
		OLR_BASE_URL="http://$$address" OLR_CHECK_THINKING=1 \
			$(SDK_RUN) tests/clients/check_anthropic.py || exit $$?; \
	done

down:
	$(COMPOSE) down
