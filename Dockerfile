# syntax=docker/dockerfile:1
# Base images are pinned by digest; Dependabot (.github/dependabot.yml) proposes updates.

FROM node:22.23.1-bookworm-slim@sha256:6c74791e557ce11fc957704f6d4fe134a7bc8d6f5ca4403205b2966bd488f6b3 AS portal-build
WORKDIR /src/web
COPY web/package*.json ./
COPY web/scripts/ scripts/
RUN npm ci
COPY web/ ./
RUN npm run build

# The Go version is pinned here and in go.mod; change both together.
FROM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/assets.go web/assets.go
COPY --from=portal-build /src/web/dist web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/olr ./cmd/olr \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mock-llm ./cmd/mock-llm \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/olr-loadtest ./cmd/olr-loadtest

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS loadtest
COPY --from=build /out/olr-loadtest /usr/local/bin/olr-loadtest
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/olr-loadtest"]

# Build with `docker build --target mock-llm -t olr-mock-llm .` when only
# the deterministic test backend is needed.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS mock-llm
COPY --from=build /out/mock-llm /usr/local/bin/mock-llm
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mock-llm"]

# Distroless static: no shell, no package manager; runs as user 65532.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS olr
COPY --from=build /out/olr /usr/local/bin/olr
# The image carries no config. Mount the site's file at this path.
ENV OLR_CONFIG=/etc/olr/config.yaml
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/olr"]
CMD ["serve"]
