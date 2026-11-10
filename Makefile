# Tools run at pinned versions without becoming dependencies of the module.
STATICCHECK := go run honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
COMPOSE     := docker compose

.PHONY: build lint vuln test up smoke down

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
	@for probe in "router 8080 /healthz" "mock-llm-a 8000 /health" "mock-llm-b 8000 /health"; do \
		set -- $$probe; \
		address="$$($(COMPOSE) port $$1 $$2)" || exit $$?; \
		curl --fail --silent --show-error --retry 20 --retry-all-errors \
			--retry-delay 1 --retry-max-time 30 --max-time 2 "http://$$address$$3" || exit $$?; \
	done

down:
	$(COMPOSE) down
