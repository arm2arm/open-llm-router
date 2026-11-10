# staticcheck is run at a pinned version without becoming a dependency of the module.
STATICCHECK := go run honnef.co/go/tools/cmd/staticcheck@v0.8.2
COMPOSE     := docker compose -f deploy/compose/docker-compose.yml

.PHONY: build lint test up down

build:
	CGO_ENABLED=0 go build -trimpath -o bin/olr ./cmd/olr

lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	$(STATICCHECK) ./...

test:
	go test -race ./...

up:
	$(COMPOSE) up --build --detach

down:
	$(COMPOSE) down
